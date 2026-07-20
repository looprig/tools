package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// TestWebSearchPrepareCallProviderEndpoints pins the requirement shape: one
// network requirement per declared provider endpoint, empty scope and grant
// pair, with a reusable target-scoped candidate.
func TestWebSearchPrepareCallProviderEndpoints(t *testing.T) {
	t.Parallel()
	provider := &fakeSearchProvider{endpoints: []Endpoint{
		{Host: "Search.Example.COM", Port: 443},
		{Host: "alt.example.com", Port: 8443},
	}}
	ws := NewWebSearch(provider)
	req, art, err := ws.PrepareCall(context.Background(), mustUUID(t), `{"query":"golang"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if len(req.Requirements) != 2 {
		t.Fatalf("Requirements = %d, want one per declared endpoint", len(req.Requirements))
	}
	wantMatches := map[string]bool{
		permission.NetworkTargetMatch("tcp", "search.example.com", 443): false,
		permission.NetworkTargetMatch("tcp", "alt.example.com", 8443):   false,
	}
	for _, r := range req.Requirements {
		if r.Kind != permission.CapabilityNetwork || r.Scope != "" {
			t.Errorf("requirement = %+v, want kind network with empty scope", r)
		}
		if r.GrantClass != "" || r.GrantTarget != "" {
			t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
		}
		if _, ok := wantMatches[r.Match]; !ok {
			t.Errorf("unexpected requirement match %q", r.Match)
		}
		wantMatches[r.Match] = true
		if len(r.Candidates) != 1 || r.Candidates[0].Match != r.Match || r.Candidates[0].GrantClass != "" {
			t.Errorf("candidates = %+v, want ONE reusable target-scoped candidate", r.Candidates)
		}
	}
	for match, seen := range wantMatches {
		if !seen {
			t.Errorf("missing requirement for %q", match)
		}
	}
	if art == nil {
		t.Fatal("artifact = nil, want a typed websearch artifact")
	}
}

// TestWebSearchPrepareCallRejects proves malformed input and an endpoint-less
// provider fail during preparation.
func TestWebSearchPrepareCallRejects(t *testing.T) {
	t.Parallel()
	good := []Endpoint{{Host: "search.example.com", Port: 443}}
	tests := map[string]struct {
		endpoints []Endpoint
		args      string
	}{
		"not json":            {endpoints: good, args: `x`},
		"missing query":       {endpoints: good, args: `{}`},
		"blank query":         {endpoints: good, args: `{"query":"   "}`},
		"no endpoints":        {endpoints: []Endpoint{}, args: `{"query":"golang"}`},
		"invalid endpoint":    {endpoints: []Endpoint{{Host: "bad host", Port: 443}}, args: `{"query":"golang"}`},
		"endpoint port range": {endpoints: []Endpoint{{Host: "ok.example.com", Port: 0}}, args: `{"query":"golang"}`},
	}
	for name, tt := range tests {
		name, tt := name, tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ws := NewWebSearch(&fakeSearchProvider{endpoints: tt.endpoints})
			if _, _, err := ws.PrepareCall(context.Background(), mustUUID(t), tt.args); err == nil {
				t.Errorf("PrepareCall(%s) error = nil, want an error", tt.args)
			}
		})
	}
}

// TestWebSearchRunConsumesArtifact proves the run path uses the PREPARED query
// and fails closed without its artifact.
func TestWebSearchRunConsumesArtifact(t *testing.T) {
	t.Parallel()
	provider := &fakeSearchProvider{endpoints: []Endpoint{{Host: "search.example.com", Port: 443}}}
	ws := NewWebSearch(provider)
	id := mustUUID(t)
	req, art, err := ws.PrepareCall(context.Background(), id, `{"query":"approved query"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	if _, err := ws.InvokableRun(ctx, `{"query":"mutated query"}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if provider.gotQuery != "approved query" {
		t.Errorf("provider saw query %q, want the PREPARED query", provider.gotQuery)
	}

	fresh := &fakeSearchProvider{endpoints: provider.endpoints}
	ws2 := NewWebSearch(fresh)
	res, err := ws2.InvokableRun(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if out := textOf(t, res); !strings.HasPrefix(out, "error:") {
		t.Fatalf("result = %q, want fail-closed without an artifact", out)
	}
	if fresh.calls != 0 {
		t.Fatal("provider was invoked without a prepared artifact")
	}
}

// TestDuckDuckGoDeclaresEndpoint pins the DuckDuckGo provider's declared
// endpoint to the host it actually scrapes: https://html.duckduckgo.com:443.
func TestDuckDuckGoDeclaresEndpoint(t *testing.T) {
	t.Parallel()
	p := NewDuckDuckGoProvider(http.DefaultClient)
	endpoints := p.Endpoints()
	if len(endpoints) != 1 || endpoints[0].Host != "html.duckduckgo.com" || endpoints[0].Port != 443 {
		t.Fatalf("Endpoints() = %+v, want exactly html.duckduckgo.com:443", endpoints)
	}
	u, err := url.Parse(ddgHTMLEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Hostname() != endpoints[0].Host {
		t.Fatalf("declared endpoint %+v does not cover the scrape target %q", endpoints[0], ddgHTMLEndpoint)
	}
}

// TestConfinedClientBlocksSecondaryTargets proves the provider's confined
// client refuses a redirect to any target outside the declared endpoints while
// following a same-target redirect.
func TestConfinedClientBlocksSecondaryTargets(t *testing.T) {
	t.Parallel()

	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/off-target":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/same-target":
			http.Redirect(w, r, "/landed", http.StatusFound)
		case "/landed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("LANDED"))
		}
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	allowed := []Endpoint{{Host: u.Hostname(), Port: port}}
	client := confinedClient(srv.Client(), allowed)

	if resp, err := client.Get(srv.URL + "/off-target"); err == nil {
		_ = resp.Body.Close()
		t.Error("off-target redirect error = nil, want fail-closed")
	}
	if leaked {
		t.Error("off-target redirect was followed (request reached the undeclared host)")
	}

	resp, err := client.Get(srv.URL + "/same-target")
	if err != nil {
		t.Fatalf("same-target redirect error = %v, want it followed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("same-target status = %d, want 200", resp.StatusCode)
	}
}
