package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
)

func setup() (*Registry, []*backend.Backend) {
	var bs []*backend.Backend
	for _, h := range []string{"a:1", "b:2"} {
		u, _ := url.Parse("http://" + h)
		bs = append(bs, backend.New(u, 1))
	}
	return New("round_robin", bs), bs
}

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status %d", rec.Code)
	}
	return rec.Body.String()
}

func TestPrometheusSeries(t *testing.T) {
	r, bs := setup()
	r.UpstreamResponse(bs[0], 200)
	r.UpstreamResponse(bs[0], 200)
	r.UpstreamResponse(bs[1], 503)
	r.UpstreamError(bs[1])
	r.Retry()
	r.StateChange(bs[1], false)
	bs[1].RecordFailure(1)
	release := bs[0].Acquire()
	defer release()

	out := scrape(t, r)
	for _, want := range []string{
		`lb_upstream_responses_total{backend="a:1",code="200"} 2`,
		`lb_upstream_responses_total{backend="b:2",code="503"} 1`,
		`lb_upstream_connection_errors_total{backend="b:2"} 1`,
		`lb_retries_total 1`,
		`lb_backend_up{backend="a:1"} 1`,
		`lb_backend_up{backend="b:2"} 0`,
		`lb_backend_active_requests{backend="a:1"} 1`,
		`lb_backend_state_changes_total{backend="b:2",to="down"} 1`,
		`# TYPE lb_http_request_duration_seconds histogram`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Every non-comment line must be `name{labels} value` or `name value`, the
// shape Prometheus expects.
func TestExpositionFormatIsWellFormed(t *testing.T) {
	r, bs := setup()
	r.UpstreamResponse(bs[0], 200)
	for _, line := range strings.Split(strings.TrimSpace(scrape(t, r)), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "lb_") {
			t.Errorf("malformed line %q", line)
		}
	}
}

func TestMiddlewareRecordsStatusAndLatency(t *testing.T) {
	r, _ := setup()
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/slow" {
			time.Sleep(30 * time.Millisecond)
		}
		if req.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			w.WriteHeader(http.StatusTeapot) // ignored, like net/http does
		}
	}))
	for _, path := range []string{"/", "/", "/missing", "/slow"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	out := scrape(t, r)
	for _, want := range []string{
		`lb_http_requests_total{code="200"} 3`,
		`lb_http_requests_total{code="404"} 1`,
		`lb_http_request_duration_seconds_count 4`,
		`lb_http_request_duration_seconds_bucket{le="0.01"} 3`, // /slow is past 10ms
		`lb_http_request_duration_seconds_bucket{le="+Inf"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `code="418"`) {
		t.Error("second WriteHeader was recorded")
	}
}

func TestStatusJSON(t *testing.T) {
	r, bs := setup()
	r.UpstreamResponse(bs[0], 200)
	r.UpstreamError(bs[1])
	r.StateChange(bs[1], false)
	bs[1].RecordFailure(1)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Strategy != "round_robin" || st.Healthy != 1 || st.Total != 2 {
		t.Errorf("status = %+v", st)
	}
	b := st.Backends[1]
	if b.Alive || b.Errors != 1 || b.Downs != 1 || st.Backends[0].Requests != 1 {
		t.Errorf("backends = %+v", st.Backends)
	}
}
