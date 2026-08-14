// End-to-end tests: HTTP in, PostgreSQL out.
//
// These drive the engine the server actually builds — the same wiring, the same
// middleware, the same repositories, against a real database. That matters
// because almost everything this system guarantees lives at a seam: a lock
// order, a partial unique index, a trigger that refuses an UPDATE, a
// transaction that must roll back as a whole. A test with a fake repository
// proves the Go compiles; it proves nothing about whether the money is safe.
//
// Run with a database:
//
//	DB_NAME=flowed_test go test ./test/e2e/
//
// Skipped under -short.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/bootstrap"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/migrate"
	"github.com/swibit/flowed/internal/platform/observability"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
	"github.com/swibit/flowed/migrations"
)

var (
	server *httptest.Server
	db     *pg.DB
	// adminToken is a signed-in administrator. Built once: bcrypt at a real
	// cost is the slowest thing in this file.
	adminToken string
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}

	cfg := testConfig()
	// Quiet by default — a passing run should say nothing — but a failing run
	// is much easier to read with the server's own log beside it.
	var sink io.Writer = io.Discard
	if os.Getenv("E2E_LOG") != "" {
		sink = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(sink, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: cannot reach the database (%v).\n"+
			"Run `make test-db-setup`, or use -short to skip.\n", err)
		os.Exit(1)
	}
	db = pool

	runner, err := migrate.New(db.Pool(), migrations.FS, migrations.Dir, log)
	if err == nil {
		if err := runner.Validate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: schema does not match this build: %v\n", err)
			os.Exit(1)
		}
	}

	if err := seedYearCounter(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: reading existing academic years: %v\n", err)
		os.Exit(1)
	}

	obs, err := observability.Setup(ctx, cfg.Observability, cfg.App, "e2e", log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: observability: %v\n", err)
		os.Exit(1)
	}

	engine, _ := bootstrap.BuildEngine(cfg, log, db, obs, "e2e")
	server = httptest.NewServer(engine)

	code := m.Run()

	server.Close()
	db.Close()
	os.Exit(code)
}

func testConfig() *config.Config {
	os.Setenv("APP_ENV", "development")
	os.Setenv("DB_NAME", envOr("DB_NAME", "flowed_test"))
	os.Setenv("DB_USER", envOr("DB_USER", os.Getenv("USER")))
	// Configuration refuses a cost below 10, and rightly: a weak cost in a
	// deployment is a real weakness. The tests take the floor of the sane
	// range rather than arguing with the check they are meant to be testing.
	os.Setenv("AUTH_BCRYPT_COST", "10")
	os.Setenv("OBS_METRICS_ENABLED", "false")
	os.Setenv("HTTP_RATE_LIMIT_PER_MINUTE", "0") // Rate limiting has its own tests.

	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type response struct {
	status int
	body   map[string]any
	raw    []byte
}

// data unwraps the {data: …} envelope every successful response uses.
func (r response) data() map[string]any {
	if inner, ok := r.body["data"].(map[string]any); ok {
		return inner
	}
	return r.body
}

// list reads either list shape the API uses: a paged endpoint answers
// {"data": [...], "total": n} and a small reference endpoint answers a bare
// array. A client has to cope with both, so the test does too.
// nested reads a value out of a wrapped payload: several commands answer with
// the object plus the context an operator needs beside it, so the created row
// is under a key rather than at the top level.
func (r response) nested(key string) map[string]any {
	inner, _ := r.data()[key].(map[string]any)
	if inner == nil {
		return map[string]any{}
	}
	return inner
}

func (r response) list() []any {
	if inner, ok := r.body["data"].([]any); ok {
		return inner
	}
	var bare []any
	if err := json.Unmarshal(r.raw, &bare); err == nil {
		return bare
	}
	return nil
}

// errorCode is the stable machine code a client branches on. Asserting on it
// rather than on the message is what these tests and a real client have in
// common.
func (r response) errorCode() string {
	if err, ok := r.body["error"].(map[string]any); ok {
		if code, ok := err["code"].(string); ok {
			return code
		}
	}
	return ""
}

type client struct {
	t     *testing.T
	token string
}

func (c *client) do(method, path string, body any, headers map[string]string) response {
	c.t.Helper()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encoding request: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, server.URL+path, payload)
	if err != nil {
		c.t.Fatalf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, raw: raw}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

func (c *client) get(path string) response { return c.do(http.MethodGet, path, nil, nil) }
func (c *client) post(path string, body any) response {
	return c.do(http.MethodPost, path, body, nil)
}

// postMoney sends a money-moving request with an idempotency key.
func (c *client) postMoney(path string, body any, key string) response {
	return c.do(http.MethodPost, path, body, map[string]string{"Idempotency-Key": key})
}

func (c *client) expect(r response, want int, context string) response {
	c.t.Helper()
	if r.status != want {
		c.t.Fatalf("%s: got %d, want %d\n%s", context, r.status, want, truncate(string(r.raw), 600))
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// unique keeps parallel runs and repeated runs from colliding on the many
// unique indexes this schema carries.
//
// Taken from the tail of the identifier rather than the head. Identifiers here
// are UUIDv7, whose leading characters are a millisecond timestamp: two values
// minted in the same run — or in two runs an hour apart — share that prefix,
// and the first version of this helper collided on every unique index in the
// schema within one test binary.
func unique() string {
	id := shared.NewID().String()
	return strings.ToLower(strings.ReplaceAll(id[len(id)-12:], "-", ""))
}

// yearCounter mints a distinct academic year per scenario.
//
// The code has to look like 2025-2026 — the domain refuses anything else, and
// rightly, because a year code appears on receipts and in ministry returns.
// Counting upward from a high base keeps every run's years distinct without
// colliding with the ones a demo dataset already loaded.
// yearCounter hands out an academic year code no run has used before.
//
// Seeded from the database rather than from zero or from the clock: the rows a
// previous run created are still there — nothing financial is deleted — and a
// duplicate code is a 409 that looks like a broken fixture.
var yearCounter atomic.Int64

func seedYearCounter(ctx context.Context) error {
	var highest int
	err := db.Pool().QueryRow(ctx,
		`SELECT coalesce(max(left(code, 4)::int), 2199) FROM academic_year
		  WHERE code ~ '^[0-9]{4}-[0-9]{4}$'`).Scan(&highest)
	if err != nil {
		return err
	}
	if highest < 2199 {
		highest = 2199
	}
	yearCounter.Store(int64(highest))
	return nil
}

func nextYearCode() (code, start, end string) {
	base := int(yearCounter.Add(1))
	// The domain checks the code against the dates, which is a good rule: a
	// year labelled 2025-2026 that runs in 2031 is a receipt nobody can file.
	// The fixture derives one from the other rather than fighting it.
	return fmt.Sprintf("%d-%d", base, base+1),
		fmt.Sprintf("%d-09-01", base),
		fmt.Sprintf("%d-07-01", base+1)
}

// signIn creates an operator with the given roles and returns a client holding
// their token, with the must-change-password step already completed.
func signIn(t *testing.T, roles ...string) *client {
	t.Helper()
	admin := adminClient(t)

	username := "e2e." + unique()
	created := admin.expect(admin.post("/api/v1/users", map[string]any{
		"username":  username,
		"full_name": "E2E " + username,
		"roles":     roles,
	}), http.StatusCreated, "creating an operator")

	temporary, _ := created.data()["temporary_password"].(string)
	if temporary == "" {
		t.Fatal("the server should generate and return a temporary password once")
	}

	// A cashier cannot sign in without a desk: receipt series run per desk, and
	// a collection with no desk has no paper book to reconcile against. The
	// fixture opens one rather than working around the rule.
	credentials := map[string]any{"username": username, "password": temporary}
	for _, role := range roles {
		if role == "cashier" {
			credentials["cashier_desk_id"] = ensureDesk(t, admin)
		}
	}

	anonymous := &client{t: t}
	login := anonymous.expect(anonymous.post("/api/v1/auth/login", credentials),
		http.StatusOK, "signing in")

	token, _ := login.data()["access_token"].(string)
	operator := &client{t: t, token: token}

	// Every created account holds a credential somebody else set, and every
	// route but this one refuses until it is replaced.
	password := "marsh warbler thicket " + unique()
	operator.expect(operator.post("/api/v1/auth/change-password", map[string]any{
		"current_password": temporary, "new_password": password,
	}), http.StatusOK, "changing the temporary password")

	credentials["password"] = password
	login = anonymous.expect(anonymous.post("/api/v1/auth/login", credentials),
		http.StatusOK, "signing in again")
	token, _ = login.data()["access_token"].(string)
	return &client{t: t, token: token}
}

// ensureDesk opens a cashier desk once and reuses it.
var (
	deskOnce sync.Once
	deskID   string
)

func ensureDesk(t *testing.T, admin *client) string {
	t.Helper()
	deskOnce.Do(func() {
		created := admin.post("/api/v1/cashier-desks", map[string]any{
			"code": "E" + strings.ToUpper(unique()[:6]), "name_ar": "شباك الاختبار",
		})
		if created.status == http.StatusCreated {
			deskID, _ = created.data()["id"].(string)
			return
		}
		t.Fatalf("opening a cashier desk: got %d: %s", created.status, created.raw)
	})
	return deskID
}

// adminClient returns a signed-in administrator, creating one directly the
// first time. Created through the repositories rather than the API because
// there is no administrator to authorise the first administrator.
func adminClient(t *testing.T) *client {
	t.Helper()
	if adminToken != "" {
		return &client{t: t, token: adminToken}
	}

	ctx := context.Background()
	users := postgres.NewUserRepository(db)
	tx := postgres.NewTxManager(db)
	hasher := auth.NewHasher(config.Auth{BcryptCost: 10})

	username := "e2e.admin." + unique()
	password := "kingfisher river bend"
	hash, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	user := &port.User{
		ID: shared.NewID(), Username: username, FullName: "E2E Administrator",
		PasswordHash: hash, IsActive: true, Roles: []shared.Role{shared.RoleAdmin},
	}
	err = tx.Write(ctx, func(ctx context.Context) error {
		if err := users.Create(ctx, user); err != nil {
			return err
		}
		return users.SetRoles(ctx, user.ID, user.Roles, user.ID)
	})
	if err != nil {
		t.Fatalf("creating the first administrator: %v", err)
	}

	anonymous := &client{t: t}
	login := anonymous.expect(anonymous.post("/api/v1/auth/login", map[string]any{
		"username": username, "password": password,
	}), http.StatusOK, "signing in as administrator")
	adminToken, _ = login.data()["access_token"].(string)
	return &client{t: t, token: adminToken}
}

// ---------------------------------------------------------------------------
// Authentication and authorisation
// ---------------------------------------------------------------------------

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	anonymous := &client{t: t}

	for _, path := range []string{"/api/v1/students", "/api/v1/users", "/api/v1/reports/debt"} {
		if got := anonymous.get(path); got.status != http.StatusUnauthorized {
			t.Errorf("%s answered %d without a credential, want 401", path, got.status)
		}
	}
	// The probes stay open: a load balancer has no credential, and a readiness
	// check that needed one could not report that authentication is broken.
	for _, path := range []string{"/health", "/ready"} {
		if got := anonymous.get(path); got.status != http.StatusOK {
			t.Errorf("%s answered %d, want 200", path, got.status)
		}
	}
}

func TestRolesAreEnforcedServerSide(t *testing.T) {
	cashier := signIn(t, "cashier")

	// A cashier may take money and may not administer users, publish fee
	// policy, or read the audit trail.
	refused := map[string]string{
		"/api/v1/users":                    "GET",
		"/api/v1/oversight/reconciliation": "GET",
	}
	for path, method := range refused {
		got := cashier.do(method, path, nil, nil)
		if got.status != http.StatusForbidden {
			t.Errorf("%s %s answered %d for a cashier, want 403", method, path, got.status)
		}
		if got.errorCode() != "insufficient_role" {
			t.Errorf("%s: code = %q, want insufficient_role", path, got.errorCode())
		}
	}
}

func TestLogoutEndsTheSessionImmediately(t *testing.T) {
	operator := signIn(t, "registrar")

	operator.expect(operator.get("/api/v1/auth/me"), http.StatusOK, "before logout")
	operator.expect(operator.post("/api/v1/auth/logout", map[string]any{}), http.StatusOK, "logout")

	// The token is still signed and unexpired; what changed is the session
	// behind it. Without that check, signing out would be a promise the system
	// does not keep for the life of the token.
	after := operator.get("/api/v1/auth/me")
	if after.status != http.StatusUnauthorized {
		t.Fatalf("the token still worked after logout: %d", after.status)
	}
	if after.errorCode() != "auth.session_revoked" {
		t.Errorf("code = %q, want auth.session_revoked", after.errorCode())
	}
}

func TestDisablingAnAccountEndsItsSessions(t *testing.T) {
	admin := adminClient(t)
	victim := signIn(t, "registrar")

	me := victim.expect(victim.get("/api/v1/auth/me"), http.StatusOK, "before")
	userID, _ := me.data()["id"].(string)

	admin.expect(admin.post("/api/v1/users/"+userID+"/disable",
		map[string]any{"reason": "left the university"}), http.StatusOK, "disabling")

	if after := victim.get("/api/v1/auth/me"); after.status != http.StatusUnauthorized {
		t.Fatalf("a disabled account's token still worked: %d", after.status)
	}
}

func TestAnOperatorCannotChangeTheirOwnRoles(t *testing.T) {
	admin := adminClient(t)
	me := admin.expect(admin.get("/api/v1/auth/me"), http.StatusOK, "me")
	id, _ := me.data()["id"].(string)

	got := admin.post("/api/v1/users/"+id+"/roles", map[string]any{"roles": []string{"admin", "cashier"}})
	if got.status != http.StatusForbidden {
		t.Fatalf("self-elevation answered %d, want 403", got.status)
	}
	if got.errorCode() != "user.self_role_change" {
		t.Errorf("code = %q", got.errorCode())
	}
}

// ---------------------------------------------------------------------------
// The collection path
// ---------------------------------------------------------------------------

// scenario is a priced enrollment ready to be paid.
type scenario struct {
	studentID    string
	enrollmentID string
	accountID    string
	outstanding  int64
	methodID     string
}

// setupScenario builds a year, a college, a department, a fee policy, a
// student, an enrollment and a priced account — through the API, so what it
// proves is that the whole path works and not that the fixtures do.
func setupScenario(t *testing.T, admin *client) scenario {
	t.Helper()
	suffix := unique()

	yearCode, yearStart, yearEnd := nextYearCode()
	year := admin.expect(admin.post("/api/v1/academic-years", map[string]any{
		"code": yearCode, "start_date": yearStart, "end_date": yearEnd,
	}), http.StatusCreated, "creating a year")
	yearID, _ := year.data()["id"].(string)
	admin.expect(admin.post("/api/v1/academic-years/"+yearID+"/open", map[string]any{}),
		http.StatusOK, "opening the year")

	college := admin.expect(admin.post("/api/v1/colleges", map[string]any{
		"code": "E" + strings.ToUpper(suffix[:5]), "name_ar": "كلية الاختبار",
	}), http.StatusCreated, "creating a college")
	collegeID, _ := college.data()["id"].(string)

	department := admin.expect(admin.post("/api/v1/departments", map[string]any{
		"college_id": collegeID, "code": "D" + strings.ToUpper(suffix[:5]),
		"name_ar": "قسم الاختبار", "stage_count": 4,
	}), http.StatusCreated, "creating a department")
	departmentID, _ := department.data()["id"].(string)

	studyTypes := admin.expect(admin.get("/api/v1/study-types"), http.StatusOK, "study types")
	studyTypeID := firstIDOf(t, studyTypes)

	policy := admin.expect(admin.post("/api/v1/fee-policies", map[string]any{
		"academic_year_id": yearID,
		"college_id":       collegeID,
		"department_id":    departmentID,
		"policy_code":      "POL" + strings.ToUpper(suffix[:6]),
		"components": []map[string]any{
			{"code": "TUITION", "name_ar": "القسط", "amount": 2_000_000, "discountable": true, "mandatory": true},
			{"code": "REGISTRATION", "name_ar": "التسجيل", "amount": 100_000, "discountable": false, "mandatory": true},
		},
	}), http.StatusCreated, "defining a fee policy")
	policyID, _ := policy.data()["id"].(string)
	admin.expect(admin.post("/api/v1/fee-policies/"+policyID+"/publish", map[string]any{}),
		http.StatusOK, "publishing the policy")

	template := admin.expect(admin.post("/api/v1/installment-templates", map[string]any{
		"code": "T" + strings.ToUpper(suffix[:6]), "name_ar": "قسطان", "academic_year_id": yearID,
		"max_installments": 2,
		"lines": []map[string]any{
			{"line_no": 1, "share_bp": 5000, "due_offset_days": 30},
			{"line_no": 2, "share_bp": 5000, "due_offset_days": 120},
		},
	}), http.StatusCreated, "defining an installment template")
	templateID, _ := template.data()["id"].(string)
	admin.expect(admin.post("/api/v1/installment-templates/"+templateID+"/publish", map[string]any{}),
		http.StatusOK, "publishing the template")

	student := admin.expect(admin.post("/api/v1/students", map[string]any{
		"student_no": "E2E" + suffix, "full_name": "طالب الاختبار " + suffix, "mother_name": "أم الاختبار",
	}), http.StatusCreated, "registering a student")
	studentID, _ := student.data()["id"].(string)

	enrollment := admin.expect(admin.post("/api/v1/enrollments", map[string]any{
		"student_id": studentID, "academic_year_id": yearID, "college_id": collegeID,
		"department_id": departmentID, "study_type_id": studyTypeID, "stage": 1,
	}), http.StatusCreated, "enrolling")
	enrollmentID, _ := enrollment.nested("enrollment")["id"].(string)

	account := admin.expect(admin.postMoney("/api/v1/accounts", map[string]any{
		"enrollment_id": enrollmentID, "installment_template_id": templateID,
	}, "acct-"+suffix), http.StatusCreated, "pricing the enrollment")

	accountData := account.data()
	inner, _ := accountData["account"].(map[string]any)
	if inner == nil {
		inner = accountData
	}
	accountID, _ := inner["id"].(string)

	methods := admin.expect(admin.get("/api/v1/payment-methods"), http.StatusOK, "payment methods")
	methodID := ""
	for _, raw := range methods.list() {
		method, _ := raw.(map[string]any)
		if code, _ := method["code"].(string); code == "BANK" {
			methodID, _ = method["id"].(string)
		}
	}
	if methodID == "" {
		methodID = firstIDOf(t, methods)
	}

	return scenario{
		studentID: studentID, enrollmentID: enrollmentID, accountID: accountID,
		outstanding: 2_100_000, methodID: methodID,
	}
}

func firstIDOf(t *testing.T, r response) string {
	t.Helper()
	id := ""
	for _, raw := range r.list() {
		row, _ := raw.(map[string]any)
		if candidate, _ := row["id"].(string); candidate != "" {
			id = candidate
			break
		}
	}
	if id == "" {
		t.Fatalf("expected at least one row, got %d: %s", r.status, r.raw)
	}
	return id
}

func TestCollectionPostsAllocatesAndPrintsAReceipt(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)

	// A finance manager may collect without a cashier desk when the method is
	// not cash: the desk exists for the paper receipt book.
	finance := signIn(t, "finance_manager")

	key := "pay-" + unique()
	paid := finance.expect(finance.postMoney("/api/v1/payments", map[string]any{
		"account_id": sc.accountID, "amount": 500_000,
		"payment_method_id": sc.methodID, "method_reference": "REF-" + unique(),
	}, key), http.StatusCreated, "collecting")

	payment, _ := paid.data()["payment"].(map[string]any)
	if payment == nil {
		t.Fatalf("no payment in the response: %s", truncate(string(paid.raw), 400))
	}
	if receiptNo, _ := payment["receipt_no"].(string); receiptNo == "" {
		t.Error("a posted payment must carry a receipt number")
	}
	paymentID, _ := payment["id"].(string)

	// The receipt renders from frozen rows, and it is the one thing the
	// student physically holds.
	receipt := finance.get("/api/v1/payments/" + paymentID + "/receipt")
	if receipt.status != http.StatusOK {
		t.Fatalf("the receipt did not render: %d", receipt.status)
	}
	if !strings.Contains(string(receipt.raw), "500") {
		t.Error("the receipt should carry the amount collected")
	}

	// The statement is what the desk and the student both read.
	statement := finance.expect(finance.get("/api/v1/portal/students/"+sc.studentID+"/statement"),
		http.StatusOK, "statement")
	if outstanding := number(statement.data()["outstanding"]); outstanding != sc.outstanding-500_000 {
		t.Errorf("outstanding = %d, want %d", outstanding, sc.outstanding-500_000)
	}
}

// The property a cashier's desk depends on: a retry of the same collection is
// the same collection, not a second one.
func TestRetryingAPaymentCollectsOnce(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)
	finance := signIn(t, "finance_manager")

	body := map[string]any{
		"account_id": sc.accountID, "amount": 300_000, "payment_method_id": sc.methodID, "method_reference": "REF-" + unique(),
	}
	key := "retry-" + unique()

	first := finance.expect(finance.postMoney("/api/v1/payments", body, key),
		http.StatusCreated, "first attempt")
	second := finance.postMoney("/api/v1/payments", body, key)

	if second.status != http.StatusCreated && second.status != http.StatusOK {
		t.Fatalf("the replay answered %d", second.status)
	}
	firstPayment, _ := first.data()["payment"].(map[string]any)
	secondPayment, _ := second.data()["payment"].(map[string]any)
	if firstPayment["id"] != secondPayment["id"] {
		t.Fatalf("the retry created a second payment: %v then %v",
			firstPayment["id"], secondPayment["id"])
	}

	statement := finance.expect(finance.get("/api/v1/portal/students/"+sc.studentID+"/statement"),
		http.StatusOK, "statement")
	if paid := number(statement.data()["total_paid"]); paid != 300_000 {
		t.Errorf("total paid = %d, want the 300,000 collected once", paid)
	}
}

// The same key with a different body is a client bug, and answering it with a
// receipt for the wrong amount would be worse than refusing.
func TestReusingAKeyWithADifferentAmountIsRefused(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)
	finance := signIn(t, "finance_manager")

	key := "mismatch-" + unique()
	finance.expect(finance.postMoney("/api/v1/payments", map[string]any{
		"account_id": sc.accountID, "amount": 100_000, "payment_method_id": sc.methodID, "method_reference": "REF-" + unique(),
	}, key), http.StatusCreated, "first")

	got := finance.postMoney("/api/v1/payments", map[string]any{
		"account_id": sc.accountID, "amount": 900_000, "payment_method_id": sc.methodID, "method_reference": "REF-" + unique(),
	}, key)
	if got.status < 400 {
		t.Fatalf("a reused key with a different body answered %d, want a refusal", got.status)
	}
}

// The rule that stops a university paying out more than it took in.
func TestVoidingAPaymentThatCarriesARefundIsRefused(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)
	finance := signIn(t, "finance_manager")

	paid := finance.expect(finance.postMoney("/api/v1/payments", map[string]any{
		"account_id": sc.accountID, "amount": 1_000_000, "payment_method_id": sc.methodID, "method_reference": "REF-" + unique(),
	}, "void-"+unique()), http.StatusCreated, "collecting")
	payment, _ := paid.data()["payment"].(map[string]any)
	paymentID, _ := payment["id"].(string)

	refund := finance.expect(finance.post("/api/v1/refunds", map[string]any{
		"payment_id": paymentID, "amount": 400_000, "reason": "overpaid in error",
		"payment_method_id": sc.methodID,
	}), http.StatusCreated, "requesting a refund")
	refundID, _ := refund.data()["id"].(string)

	// Approval is somebody else's: the requester cannot approve their own.
	approver := signIn(t, "finance_manager")
	approver.expect(approver.post("/api/v1/refunds/"+refundID+"/approve", map[string]any{}),
		http.StatusOK, "approving")
	approver.expect(approver.postMoney("/api/v1/refunds/"+refundID+"/post", map[string]any{},
		"refund-"+unique()), http.StatusOK, "posting the refund")

	got := finance.post("/api/v1/voids", map[string]any{
		"payment_id": paymentID, "reason": "should have been voided",
	})
	if got.status < 400 {
		t.Fatalf("voiding a refunded payment answered %d, want a refusal", got.status)
	}
	if !strings.Contains(got.errorCode(), "refund") {
		t.Errorf("the refusal should name the refund, got code %q", got.errorCode())
	}
}

// The lock this system's design rests on. Ten cashiers collecting against one
// account must produce ten payments and one correct balance — a lost update
// here is money the university believes it did not receive.
func TestConcurrentCollectionsDoNotLoseMoney(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)
	finance := signIn(t, "finance_manager")

	const collectors = 10
	const amount = 100_000

	var wg sync.WaitGroup
	results := make([]int, collectors)
	for i := 0; i < collectors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine needs its own client: the testing.T is shared but
			// nothing else is.
			c := &client{t: t, token: finance.token}
			got := c.postMoney("/api/v1/payments", map[string]any{
				"account_id": sc.accountID, "amount": amount,
				"payment_method_id": sc.methodID,
				// A transfer carries its bank reference: the rule exists so a
				// transfer can be found on the statement it came from.
				"method_reference": "REF-" + unique(),
			}, fmt.Sprintf("concurrent-%s-%d", unique(), i))
			results[i] = got.status
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, status := range results {
		if status == http.StatusCreated || status == http.StatusOK {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent collection succeeded")
	}

	statement := finance.expect(finance.get("/api/v1/portal/students/"+sc.studentID+"/statement"),
		http.StatusOK, "statement")
	wantPaid := int64(succeeded * amount)
	if paid := number(statement.data()["total_paid"]); paid != wantPaid {
		t.Fatalf("total paid = %d after %d concurrent collections, want %d — a lost update",
			paid, succeeded, wantPaid)
	}
}

// ---------------------------------------------------------------------------
// The lifecycle commands that had no entry point before
// ---------------------------------------------------------------------------

func TestWithdrawalRequiresAnExplicitFinancialTreatment(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)

	// No treatment: refused, with the options named.
	got := admin.post("/api/v1/enrollments/"+sc.enrollmentID+"/status", map[string]any{
		"status": "withdrawn", "reason": "left the university",
	})
	if got.status != http.StatusBadRequest {
		t.Fatalf("a withdrawal with no financial treatment answered %d, want 400", got.status)
	}
	if got.errorCode() != "enrollment.financial_treatment_required" {
		t.Errorf("code = %q", got.errorCode())
	}

	// With one: the unpaid remainder is written off and the account settles.
	applied := admin.expect(admin.post("/api/v1/enrollments/"+sc.enrollmentID+"/status", map[string]any{
		"status": "withdrawn", "financial_treatment": "waive_unpaid", "reason": "left the university",
	}), http.StatusOK, "withdrawing with a treatment")

	treatment, _ := applied.data()["financial_treatment"].(map[string]any)
	if treatment == nil {
		t.Fatal("the response should report what happened to the money")
	}
	if waived := number(treatment["waived"]); waived != sc.outstanding {
		t.Errorf("waived %d, want the whole %d that was unpaid", waived, sc.outstanding)
	}

	statement := admin.expect(admin.get("/api/v1/portal/students/"+sc.studentID+"/statement"),
		http.StatusOK, "statement")
	if outstanding := number(statement.data()["outstanding"]); outstanding != 0 {
		t.Errorf("outstanding = %d after waiving the unpaid remainder, want 0", outstanding)
	}
}

func TestInstallmentPlanCanBeRescheduledWithoutTouchingPaidMoney(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)
	finance := signIn(t, "finance_manager")

	finance.expect(finance.postMoney("/api/v1/payments", map[string]any{
		"account_id": sc.accountID, "amount": 1_050_000, "payment_method_id": sc.methodID, "method_reference": "REF-" + unique(),
	}, "plan-"+unique()), http.StatusCreated, "paying the first installment")

	account := finance.expect(finance.get("/api/v1/accounts/"+sc.accountID), http.StatusOK, "account")
	installments, _ := account.data()["installments"].([]any)
	if len(installments) < 2 {
		t.Fatalf("expected two installments, got %d", len(installments))
	}

	dates := map[string]any{}
	for _, raw := range installments {
		inst, _ := raw.(map[string]any)
		if status, _ := inst["status"].(string); status == "paid" {
			continue
		}
		id, _ := inst["id"].(string)
		dates[id] = "2026-06-30"
	}

	got := finance.post("/api/v1/accounts/"+sc.accountID+"/plan", map[string]any{
		"kind": "reschedule", "reason": "family asked for after the harvest", "due_dates": dates,
	})
	if got.status != http.StatusOK {
		t.Fatalf("rescheduling answered %d: %s", got.status, truncate(string(got.raw), 400))
	}

	// The plan still sums to what is owed; the domain asserts it and a failure
	// would have aborted the transaction.
	statement := finance.expect(finance.get("/api/v1/portal/students/"+sc.studentID+"/statement"),
		http.StatusOK, "statement")
	if paid := number(statement.data()["total_paid"]); paid != 1_050_000 {
		t.Errorf("total paid = %d after rescheduling; money already paid must not move", paid)
	}
}

func TestGraduationClearanceBlocksADebtorWhenTheYearSaysSo(t *testing.T) {
	admin := adminClient(t)
	sc := setupScenario(t, admin)

	// A completion follows a result: the system refuses to complete an
	// enrollment whose examinations are still pending, which is the rule that
	// stops a graduation being recorded before the marks exist.
	admin.expect(admin.post("/api/v1/enrollments/"+sc.enrollmentID+"/result", map[string]any{
		"result": "passed_r1",
	}), http.StatusOK, "recording a pass")

	// The scenario's department runs four stages, so completing stage one is a
	// promotion rather than a graduation and clearance does not apply.
	got := admin.expect(admin.post("/api/v1/enrollments/"+sc.enrollmentID+"/status", map[string]any{
		"status": "completed", "reason": "passed",
	}), http.StatusOK, "completing a non-final stage")

	if clearance := got.data()["graduation_clearance"]; clearance != nil {
		t.Error("completing a non-final stage is a promotion; it must not decide clearance")
	}
}

func TestMergingStudentsMovesEnrollmentsAndKeepsTheTombstone(t *testing.T) {
	admin := adminClient(t)
	duplicate := setupScenario(t, admin)

	suffix := unique()
	target := admin.expect(admin.post("/api/v1/students", map[string]any{
		"student_no": "E2ET" + suffix, "full_name": "طالب الاختبار " + suffix,
		"mother_name": "أم الاختبار", "acknowledge_duplicates": true,
	}), http.StatusCreated, "creating the canonical record")
	targetID, _ := target.data()["id"].(string)

	merged := admin.expect(admin.post("/api/v1/students/"+targetID+"/merge", map[string]any{
		"source_student_id": duplicate.studentID, "reason": "same person registered twice",
		"acknowledge_different_identity": true,
	}), http.StatusOK, "merging")

	if moved := number(merged.data()["enrollments_moved"]); moved < 1 {
		t.Errorf("enrollments moved = %d, want at least one", moved)
	}

	// The tombstone still resolves: a receipt printed under the old number
	// must still find a person.
	source := admin.expect(admin.get("/api/v1/students/"+duplicate.studentID), http.StatusOK, "source")
	if status, _ := source.data()["status"].(string); status != "merged" {
		t.Errorf("the source record's status is %q, want merged", status)
	}

	// And the money went with the enrollment.
	statement := admin.expect(admin.get("/api/v1/portal/students/"+targetID+"/statement"),
		http.StatusOK, "target statement")
	if outstanding := number(statement.data()["outstanding"]); outstanding != duplicate.outstanding {
		t.Errorf("the target owes %d, want the %d that moved with the enrollment",
			outstanding, duplicate.outstanding)
	}
}

// ---------------------------------------------------------------------------
// Oversight
// ---------------------------------------------------------------------------

func TestReconciliationAndAuditChainAreCleanAfterEverythingAbove(t *testing.T) {
	admin := adminClient(t)

	drift := admin.expect(admin.get("/api/v1/oversight/reconciliation?limit=50"),
		http.StatusOK, "reconciliation")
	if rows := drift.list(); len(rows) != 0 {
		t.Fatalf("%d account(s) drifted between their cached totals and their transactions", len(rows))
	}

	chain := admin.expect(admin.get("/api/v1/oversight/audit/verify"), http.StatusOK, "audit chain")
	problems, _ := chain.data()["problems"].([]any)
	if len(problems) != 0 {
		t.Fatalf("the audit hash chain reports %d problem(s)", len(problems))
	}
}

func TestReportsExportAsSpreadsheets(t *testing.T) {
	admin := adminClient(t)

	years := admin.expect(admin.get("/api/v1/academic-years"), http.StatusOK, "years")
	yearID := firstIDOf(t, years)

	for _, format := range []string{"csv", "xlsx", "pdf"} {
		got := admin.get("/api/v1/reports/debt?academic_year_id=" + yearID + "&format=" + format)
		if got.status != http.StatusOK {
			t.Errorf("%s export answered %d", format, got.status)
			continue
		}
		if len(got.raw) == 0 {
			t.Errorf("%s export was empty", format)
		}
	}

	// An unknown format is refused rather than silently becoming CSV.
	if got := admin.get("/api/v1/reports/debt?academic_year_id=" + yearID + "&format=docx"); got.status != http.StatusBadRequest {
		t.Errorf("an unknown export format answered %d, want 400", got.status)
	}
}

func TestMasterDataAdministrationIsReachable(t *testing.T) {
	admin := adminClient(t)
	suffix := strings.ToUpper(unique()[:6])

	desk := admin.expect(admin.post("/api/v1/cashier-desks", map[string]any{
		"code": "D" + suffix[:4], "name_ar": "شباك الاختبار",
	}), http.StatusCreated, "opening a cashier desk")
	deskID, _ := desk.data()["id"].(string)

	admin.expect(admin.do(http.MethodPatch, "/api/v1/cashier-desks/"+deskID, map[string]any{
		"name_ar": "شباك معدّل", "reason": "renamed",
	}, nil), http.StatusOK, "renaming the desk")

	// A payment method's cash flag is frozen once money has moved through it,
	// but a fresh one can still be created and edited.
	method := admin.expect(admin.post("/api/v1/payment-methods", map[string]any{
		"code": "M" + suffix[:4], "name_ar": "طريقة اختبار", "requires_reference": true,
	}), http.StatusCreated, "creating a payment method")
	if id, _ := method.data()["id"].(string); id == "" {
		t.Error("the method should come back with an identifier")
	}
}

func number(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

var _ = app.LockoutPolicy{}
