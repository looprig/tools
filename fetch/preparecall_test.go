package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// runPreparedFetch prepares and runs one Fetch call; a preparation error is
// returned as an "error: ..." string so rejection rows share the helper.
func runPreparedFetch(t *testing.T, f *Fetch, argsJSON string) string {
	t.Helper()
	id := mustUUID(t)
	req, art, err := f.PrepareCall(context.Background(), id, argsJSON)
	if err != nil {
		return "error: " + err.Error()
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := f.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() unexpected Go error = %v", err)
	}
	return textOf(t, res)
}

// TestFetchPrepareCallRequirement pins the ONE shared network requirement Fetch
// emits: kind network, empty scope, host/port target match, EMPTY grant pair
// (Fetch enforces its approved target itself), and a reusable target candidate.
func TestFetchPrepareCallRequirement(t *testing.T) {
	t.Parallel()
	f := NewFetch(http.DefaultClient)
	req, art, err := f.PrepareCall(context.Background(), mustUUID(t), `{"url":"HTTPS://ExAmPlE.CoM./a/b?q=1","method":"get"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want ONE shared network requirement", len(req.Requirements))
	}
	r := req.Requirements[0]
	wantMatch := permission.NetworkTargetMatch("tcp", "example.com", 443)
	if r.Kind != permission.CapabilityNetwork || r.Scope != "" || r.Match != wantMatch {
		t.Errorf("requirement = %+v, want kind network, empty scope, match %q", r, wantMatch)
	}
	if r.GrantClass != "" || r.GrantTarget != "" {
		t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
	}
	if len(r.Candidates) != 1 || r.Candidates[0].Match != wantMatch || r.Candidates[0].GrantClass != "" {
		t.Errorf("candidates = %+v, want ONE reusable target-scoped candidate with an empty grant pair", r.Candidates)
	}
	if art == nil {
		t.Fatal("artifact = nil, want a typed fetch artifact")
	}
}

// TestFetchPrepareCallNormalization proves method, scheme, host, and port are
// normalized ONCE at preparation: default ports by scheme, lowercased host with
// the trailing dot stripped, and an explicit port honored.
func TestFetchPrepareCallNormalization(t *testing.T) {
	t.Parallel()
	f := NewFetch(http.DefaultClient)
	tests := []struct {
		name      string
		args      string
		wantMatch string
	}{
		{name: "https default port", args: `{"url":"https://Example.com/x","method":"GET"}`, wantMatch: "tcp://example.com:443"},
		{name: "http default port", args: `{"url":"http://example.com","method":"GET"}`, wantMatch: "tcp://example.com:80"},
		{name: "explicit port", args: `{"url":"http://example.com:8080/x","method":"POST"}`, wantMatch: "tcp://example.com:8080"},
		{name: "trailing dot host", args: `{"url":"https://example.com./x","method":"GET"}`, wantMatch: "tcp://example.com:443"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, _, err := f.PrepareCall(context.Background(), mustUUID(t), tt.args)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			if req.Requirements[0].Match != tt.wantMatch {
				t.Errorf("match = %q, want %q", req.Requirements[0].Match, tt.wantMatch)
			}
		})
	}
}

// TestFetchPrepareCallRejects proves invalid input fails during preparation and
// never reaches evaluation or the network.
func TestFetchPrepareCallRejects(t *testing.T) {
	t.Parallel()
	f := NewFetch(http.DefaultClient)
	for name, args := range map[string]string{
		"not json":          `x`,
		"missing url":       `{"method":"GET"}`,
		"missing method":    `{"url":"https://example.com"}`,
		"invalid method":    `{"url":"https://example.com","method":"PUT"}`,
		"file scheme":       `{"url":"file:///etc/passwd","method":"GET"}`,
		"no host":           `{"url":"https:///x","method":"GET"}`,
		"port out of range": `{"url":"https://example.com:99999/x","method":"GET"}`,
	} {
		name, args := name, args
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := f.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
				t.Errorf("PrepareCall(%s) error = nil, want an error", args)
			}
		})
	}
}

// TestFetchRunFailsClosedWithoutArtifact proves the run path requires the
// prepared artifact and performs no request without it.
func TestFetchRunFailsClosedWithoutArtifact(t *testing.T) {
	t.Parallel()
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f := NewFetch(srv.Client())
	res, err := f.InvokableRun(context.Background(), `{"url":"`+srv.URL+`","method":"GET"}`)
	if err != nil {
		t.Fatalf("InvokableRun() unexpected Go error = %v", err)
	}
	if got := textOf(t, res); !strings.HasPrefix(got, "error:") {
		t.Fatalf("result = %q, want fail-closed without an artifact", got)
	}
	if hit {
		t.Fatal("request was performed without a prepared artifact")
	}
}

// TestFetchRedirectFailsClosed proves a redirect to any target not covered by
// the approved requirement is refused, while a same-target redirect (same
// scheme, host, and port) is followed.
func TestFetchRedirectFailsClosed(t *testing.T) {
	t.Parallel()

	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("SECONDARY-TARGET-BODY"))
	}))
	t.Cleanup(other.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/off-target":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case "/same-target":
			http.Redirect(w, r, "/landed", http.StatusFound)
		case "/landed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("LANDED"))
		}
	}))
	t.Cleanup(srv.Close)

	f := NewFetch(srv.Client())

	got := runPreparedFetch(t, f, `{"url":"`+srv.URL+`/off-target","method":"GET"}`)
	if !strings.Contains(got, "error") || strings.Contains(got, "SECONDARY-TARGET-BODY") {
		t.Errorf("off-target redirect result = %q, want a fail-closed error without the secondary body", got)
	}
	if leaked {
		t.Error("off-target redirect was followed (request reached the unapproved host)")
	}

	got = runPreparedFetch(t, f, `{"url":"`+srv.URL+`/same-target","method":"GET"}`)
	if !strings.Contains(got, "200") || !strings.Contains(got, "LANDED") {
		t.Errorf("same-target redirect result = %q, want it followed", got)
	}
}

// TestFetchRunConsumesArtifactNotRawJSON proves execution performs the PREPARED
// request even when the raw args are mutated afterwards.
func TestFetchRunConsumesArtifactNotRawJSON(t *testing.T) {
	t.Parallel()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f := NewFetch(srv.Client())
	id := mustUUID(t)
	req, art, err := f.PrepareCall(context.Background(), id, `{"url":"`+srv.URL+`/approved","method":"GET"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	if _, err := f.InvokableRun(ctx, `{"url":"`+srv.URL+`/mutated","method":"GET"}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if gotPath != "/approved" {
		t.Errorf("server saw path %q, want the PREPARED path /approved", gotPath)
	}
}
