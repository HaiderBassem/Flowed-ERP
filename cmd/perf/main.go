// Command perf measures what the API does under load, against a real server
// and a real database.
//
// It exists because "it feels fast" is not a number, and because the numbers
// that matter to a university are not throughput: they are how long a cashier
// waits for a balance while a report is running, and whether the ninety-ninth
// request of the morning is the one that times out. So this reports p50, p95
// and p99 per endpoint, and the error rate beside them — a fast endpoint that
// is failing is not a fast endpoint.
//
//	perf -base http://localhost:8080 -user admin -password ... -duration 30s -workers 8
//
// Each worker runs the whole scenario in a loop: read a balance, search for a
// student, list installments, run a report. That mix is deliberate. Measuring
// one endpoint at a time hides the interaction that actually hurts — the report
// that holds a connection while the desks queue behind it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
)

func main() {
	var (
		base     = flag.String("base", "http://localhost:8080", "base URL of a running server")
		username = flag.String("user", "admin", "operator to sign in as")
		password = flag.String("password", "", "that operator's password")
		duration = flag.Duration("duration", 30*time.Second, "how long to run")
		workers  = flag.Int("workers", 8, "concurrent callers")
		warmup   = flag.Duration("warmup", 3*time.Second, "discarded before measuring")
		jsonOut  = flag.String("json", "", "also write the results to this file")
		scenario = flag.String("scenario", "mixed", "desk, reports or mixed")
	)
	flag.Parse()

	if *password == "" {
		*password = os.Getenv("PERF_PASSWORD")
	}
	if *password == "" {
		fmt.Fprintln(os.Stderr, "a password is required: -password or PERF_PASSWORD")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := &runner{
		base:     *base,
		client:   &http.Client{Timeout: 30 * time.Second},
		scenario: *scenario,
	}
	if err := runner.signIn(ctx, *username, *password); err != nil {
		fmt.Fprintf(os.Stderr, "signing in: %v\n", err)
		os.Exit(1)
	}
	if err := runner.discoverFixtures(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "finding something to read: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("flowed perf: %s scenario, %d workers, %s (after %s warm-up), against %s\n",
		*scenario, *workers, *duration, *warmup, *base)

	results := runner.run(ctx, *workers, *duration, *warmup)
	report(results)

	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, results); err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", *jsonOut, err)
			os.Exit(1)
		}
		fmt.Printf("\nwritten to %s\n", *jsonOut)
	}

	// A non-zero exit on errors, so this can gate a release rather than only
	// inform one.
	for _, r := range results {
		if r.Errors > 0 {
			os.Exit(1)
		}
	}
}

type runner struct {
	base   string
	client *http.Client
	token  string

	studentID string
	accountID string
	yearID    string
	scenario  string
}

func (r *runner) signIn(ctx context.Context, username, password string) error {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.base+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered %d: %s", resp.StatusCode, payload)
	}

	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return err
	}
	r.token = answer.AccessToken
	if r.token == "" {
		return fmt.Errorf("no access token in the sign-in response")
	}
	return nil
}

// discoverFixtures finds a student and an account to read.
//
// Read from the running system rather than passed in: a perf run that has to
// be given identifiers is one nobody runs, and identifiers hard-coded from a
// developer's database are the reason perf suites rot.
func (r *runner) discoverFixtures(ctx context.Context) error {
	var students struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, "/api/v1/students?limit=1", &students); err != nil {
		return err
	}
	if len(students.Data) == 0 {
		return fmt.Errorf("no students; run `api perf-seed` first")
	}
	r.studentID = students.Data[0].ID

	var years []struct {
		ID string `json:"id"`
	}
	if err := r.getJSON(ctx, "/api/v1/academic-years", &years); err != nil {
		return err
	}
	if len(years) > 0 {
		r.yearID = years[0].ID
	}

	var statement struct {
		Accounts []struct {
			AccountID string `json:"account_id"`
		} `json:"accounts"`
	}
	if err := r.getJSON(ctx, "/api/v1/portal/students/"+r.studentID+"/statement", &statement); err != nil {
		return err
	}
	if len(statement.Accounts) > 0 {
		r.accountID = statement.Accounts[0].AccountID
	}
	return nil
}

func (r *runner) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d: %s", path, resp.StatusCode, truncate(body))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}

// call is one measured endpoint.
type call struct {
	name string
	path string
}

// calls builds the scenario.
//
// Three of them, because they answer different questions. "desk" is what a
// cashier waits for with a student in front of them, and it is the number that
// decides whether the queue moves. "reports" is the finance office's morning.
// "mixed" runs both at once, which is the only way to see the interaction that
// actually hurts: a report holding a connection while the desks queue behind
// it. Measuring endpoints one at a time hides exactly that.
func (r *runner) calls() []call {
	desk := []call{
		// The term goes in `q`. An earlier version of this file sent `search`,
		// which the API ignores — so it measured an unfiltered listing of every
		// student and called it a search. Both are worth measuring, and they
		// are different questions.
		{"student search (Arabic)", "/api/v1/students?q=محمد&limit=20"},
		{"student by number", "/api/v1/students?q=PERF00000001&limit=5"},
		{"student list (no filter)", "/api/v1/students?limit=20"},
		{"student statement", "/api/v1/portal/students/" + r.studentID + "/statement"},
	}
	if r.accountID != "" {
		// The account detail carries the plan and the payments with it — there
		// is no separate installments or payments-by-account endpoint, and the
		// first version of this file measured two 404s.
		desk = append(desk,
			call{"account detail", "/api/v1/accounts/" + r.accountID},
			call{"student audit trail", "/api/v1/students/" + r.studentID + "/audit?limit=20"})
	}

	if r.yearID != "" {
		// The screen an operator actually opens: this year's students, not
		// every student the university has ever had.
		desk = append(desk, call{"student list (this year)",
			"/api/v1/students?academic_year_id=" + r.yearID + "&limit=20"})
	}

	var reports []call
	if r.yearID != "" {
		reports = []call{
			{"debt report", "/api/v1/reports/debt?academic_year_id=" + r.yearID + "&limit=50"},
			{"department summary", "/api/v1/reports/departments?academic_year_id=" + r.yearID},
			{"aging report", "/api/v1/reports/aging?academic_year_id=" + r.yearID},
			{"installment report", "/api/v1/reports/installments?academic_year_id=" + r.yearID + "&limit=50"},
		}
	}

	switch r.scenario {
	case "desk":
		return desk
	case "reports":
		return reports
	default:
		return append(desk, reports...)
	}
}

// result is one endpoint's measurements.
type result struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MaxMs    float64 `json:"max_ms"`
	RPS      float64 `json:"rps"`
}

func (r *runner) run(ctx context.Context, workers int, duration, warmup time.Duration) []result {
	calls := r.calls()

	type sample struct {
		index int
		took  time.Duration
		ok    bool
	}

	samples := make(chan sample, 4096)
	measuring := make(chan struct{})

	var wg sync.WaitGroup
	runCtx, cancel := context.WithTimeout(ctx, warmup+duration)
	defer cancel()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				for i, c := range calls {
					if runCtx.Err() != nil {
						return
					}
					started := time.Now()
					err := r.getJSON(runCtx, c.path, nil)
					took := time.Since(started)

					// A request cut short because the run ended is not a
					// failure of the server, and counting it as one turned a
					// clean run into a red one on the last tick.
					if runCtx.Err() != nil {
						return
					}

					select {
					case <-measuring:
						// Warm-up samples are discarded rather than averaged
						// in: the first requests pay for connection set-up and
						// a cold page cache, and a p99 that is really "the
						// first request" measures nothing about the system.
						samples <- sample{index: i, took: took, ok: err == nil}
					default:
					}
				}
			}
		}()
	}

	go func() {
		time.Sleep(warmup)
		close(measuring)
	}()

	collected := make([][]time.Duration, len(calls))
	errors := make([]int, len(calls))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s := range samples {
			if s.ok {
				collected[s.index] = append(collected[s.index], s.took)
			} else {
				errors[s.index]++
			}
		}
	}()

	wg.Wait()
	close(samples)
	<-done

	results := make([]result, 0, len(calls))
	for i, c := range calls {
		times := collected[i]
		sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
		res := result{
			Name:     c.name,
			Path:     c.path,
			Requests: len(times) + errors[i],
			Errors:   errors[i],
		}
		if len(times) > 0 {
			res.P50Ms = ms(percentile(times, 0.50))
			res.P95Ms = ms(percentile(times, 0.95))
			res.P99Ms = ms(percentile(times, 0.99))
			res.MaxMs = ms(times[len(times)-1])
			res.RPS = float64(len(times)) / duration.Seconds()
		}
		results = append(results, res)
	}
	return results
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	// Nearest-rank: with a few thousand samples the interpolated variants
	// differ in the third decimal and are harder to explain.
	index := int(float64(len(sorted))*p) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func report(results []result) {
	fmt.Printf("\n%-26s %8s %7s %8s %8s %8s %8s\n",
		"endpoint", "requests", "errors", "p50 ms", "p95 ms", "p99 ms", "req/s")
	fmt.Println(repeat('-', 80))
	for _, r := range results {
		fmt.Printf("%-26s %8d %7d %8.1f %8.1f %8.1f %8.1f\n",
			r.Name, r.Requests, r.Errors, r.P50Ms, r.P95Ms, r.P99Ms, r.RPS)
	}
}

func writeJSON(path string, results []result) error {
	encoded, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

func repeat(c rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = c
	}
	return string(out)
}

func truncate(body []byte) string {
	const limit = 200
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…"
}
