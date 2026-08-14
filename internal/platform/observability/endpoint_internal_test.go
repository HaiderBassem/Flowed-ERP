package observability

import "testing"

// The exporter posts to the URL exactly as configured. A base URL with no path
// therefore posts to "/", which a healthy collector rejects — and it rejects
// it quietly, so the symptom is spans that never arrive from a service whose
// logs say tracing is on.
func TestTraceEndpointCompletesTheSignalPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare host and port", "http://collector:4318", "http://collector:4318/v1/traces"},
		{"trailing slash", "http://collector:4318/", "http://collector:4318/v1/traces"},
		{"https", "https://otel.university.edu.iq:4318", "https://otel.university.edu.iq:4318/v1/traces"},
		{"path already given", "http://collector:4318/v1/traces", "http://collector:4318/v1/traces"},
		{"behind a reverse proxy prefix", "https://gw.example/otel/v1/traces", "https://gw.example/otel/v1/traces"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := traceEndpoint(tc.in)
			if err != nil {
				t.Fatalf("traceEndpoint(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("traceEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A misconfigured endpoint must fail at start-up rather than silently drop
// every span.
func TestTraceEndpointRejectsWhatIsNotAURL(t *testing.T) {
	for _, in := range []string{"", "collector:4318", "/v1/traces", "localhost"} {
		if got, err := traceEndpoint(in); err == nil {
			t.Errorf("traceEndpoint(%q) = %q, want an error", in, got)
		}
	}
}
