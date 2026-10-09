package backend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adminAuditLines returns the buffered audit lines written by the admin API hook.
func adminAuditLines(app *App) []string {
	var lines []string
	for _, entry := range app.Logger.Entries(0) {
		if strings.HasPrefix(entry.Message, `admin "`) {
			lines = append(lines, entry.Message)
		}
	}
	return lines
}

// TestAdminAPIAuditLogsStateChangingCalls covers the trail the panel used to leave
// nowhere: who changed what, and which attempts were rejected.
func TestAdminAPIAuditLogsStateChangingCalls(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		before := len(adminAuditLines(app))

		rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream",
			map[string]any{"name": "x", "url": "not-a-url", "password": "super-secret"}, token)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}

		lines := adminAuditLines(app)
		if len(lines) != before+1 {
			t.Fatalf("audit lines = %d, want %d: %v", len(lines), before+1, lines)
		}
		line := lines[len(lines)-1]
		if !strings.Contains(line, `admin "admin":`) {
			t.Fatalf("audit line should name the acting account: %q", line)
		}
		if !strings.Contains(line, "POST /admin/api/upstream") || !strings.Contains(line, "400") {
			t.Fatalf("audit line should carry method, path and status: %q", line)
		}
		if strings.Contains(line, "super-secret") {
			t.Fatalf("audit line leaked the request body: %q", line)
		}
	})
}

func TestAdminAPIAuditSkipsSuccessfulReadsButLogsRejections(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		before := len(adminAuditLines(app))

		rr := doJSONRequest(t, handler, http.MethodGet, "/admin/api/status", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if got := len(adminAuditLines(app)); got != before {
			t.Fatalf("successful read should stay out of the audit trail: %v", adminAuditLines(app))
		}

		rr = doJSONRequest(t, handler, http.MethodGet, "/admin/api/status", nil, "not-a-real-token")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
		lines := adminAuditLines(app)
		if len(lines) != before+1 {
			t.Fatalf("rejected read was not logged: %v", lines)
		}
		if !strings.Contains(lines[len(lines)-1], "unknown token") {
			t.Fatalf("audit line should flag the unusable token: %q", lines[len(lines)-1])
		}
	})
}

func TestAdminAPIAuditRecordsNonAdminActor(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users",
			map[string]any{"username": "bob", "password": "bob12345"}, adminToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create user: status=%d body=%s", rr.Code, rr.Body.String())
		}
		userToken := loginTokenAs(t, handler, "bob", "bob12345")
		before := len(adminAuditLines(app))

		rr = doJSONRequest(t, handler, http.MethodPost, "/admin/api/users",
			map[string]any{"username": "mallory", "password": "x"}, userToken)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rr.Code)
		}
		lines := adminAuditLines(app)
		if len(lines) != before+1 {
			t.Fatalf("rejected non-admin write was not logged: %v", lines)
		}
		line := lines[len(lines)-1]
		if !strings.Contains(line, `admin "bob (role:user)":`) {
			t.Fatalf("audit line should name the account and its role: %q", line)
		}
	})
}

// TestCredentialEndpointHasNoCrossOriginGrant keeps the login endpoint out of reach of
// a page on another origin. The rate limiter counts real failures only, but a form post
// needs no preflight and no CORS grant to cause one, so the endpoint would still be a way
// to lock a visitor out of their own server. Denying the grant leaves the JSON body type,
// which does require a preflight, as the only way in.
func TestCredentialEndpointHasNoCrossOriginGrant(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		for _, target := range []string{"/Users/AuthenticateByName", "/emby/Users/AuthenticateByName"} {
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"Username":"admin","Pw":"wrong"}`))
			req.Header.Set("Origin", "https://evil.example")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("%s: Access-Control-Allow-Origin = %q, want it unset", target, got)
			}
		}

		// Every other Emby API path keeps its grant: the clients rely on it.
		req := httptest.NewRequest(http.MethodGet, "/System/Info/Public", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
		}
	})
}

func TestNormalizeEmbyPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/emby/Users/authenticatebyname", "/Users/AuthenticateByName"},
		{"/Users/authenticatebyname", "/Users/AuthenticateByName"},
		{"/emby/users/authenticatebyname", "/Users/AuthenticateByName"},
		{"/emby/System/Info/Public", "/System/Info/Public"},
		{"/system/info/public", "/System/Info/Public"},
		{"/emby", "/"},
		{"/emby/", "/"},
		{"/admin/api/status", "/admin/api/status"},
		{"/favicon.ico", "/favicon.ico"},
		{"/emby/Users/alice123/views", "/Users/alice123/Views"},
		{"/emby/users/alice123/items/resume", "/Users/alice123/Items/Resume"},
		{"/emby/sessions/playing/progress", "/Sessions/Playing/Progress"},
	}
	for _, tc := range tests {
		got := normalizeEmbyPath(tc.input)
		if got != tc.expected {
			t.Errorf("normalizeEmbyPath(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestAuthenticateByNameCaseInsensitiveAndForm(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		// Test lowercase route as used by AfuseKt
		req := httptest.NewRequest(http.MethodPost, "/emby/Users/authenticatebyname",
			strings.NewReader(`{"Username":"admin","Pw":"secret"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("lowercase path auth status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}

		// Test form urlencoded
		reqForm := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName",
			strings.NewReader(`Username=admin&Pw=secret`))
		reqForm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recForm := httptest.NewRecorder()
		handler.ServeHTTP(recForm, reqForm)
		if recForm.Code != http.StatusOK {
			t.Fatalf("form auth status = %d, want 200, body: %s", recForm.Code, recForm.Body.String())
		}
	})
}
