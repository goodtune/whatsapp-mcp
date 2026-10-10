package oauth

import (
	"html/template"
	"log"
	"net/http"
)

var pages = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.}} · WhatsApp MCP</title>
<style>
  :root { color-scheme: light dark; --fg: #1a1a1a; --muted: #5c5c5c; --bg: #f6f6f4; --card: #fff; --line: #ddd; --accent: #1f7a4d; }
  @media (prefers-color-scheme: dark) { :root { --fg: #eee; --muted: #aaa; --bg: #161616; --card: #202020; --line: #333; --accent: #4cc38a; } }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg);
         font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; padding: 16px; box-sizing: border-box; }
  main { background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 28px; max-width: 420px; width: 100%; box-sizing: border-box; }
  h1 { font-size: 1.25rem; margin: 0 0 12px; }
  p { margin: 0 0 12px; color: var(--muted); }
  strong, code { color: var(--fg); overflow-wrap: anywhere; }
  .actions { display: flex; gap: 8px; margin-top: 20px; }
  button { flex: 1; font: inherit; padding: 10px; border-radius: 8px; border: 1px solid var(--line); background: transparent; color: var(--fg); cursor: pointer; }
  button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
</style></head><body><main>{{end}}

{{define "consent"}}{{template "head" "Allow access"}}
<h1>Allow access to your WhatsApp?</h1>
<p><strong>{{if .ClientName}}{{.ClientName}}{{else}}An MCP client{{end}}</strong> wants to read and send WhatsApp messages as you.</p>
<p>You're signed in as <strong>{{.Login}}</strong> on GitHub. If you allow it, you'll be sent back to <code>{{.RedirectHost}}</code>.</p>
<p>Only allow this if you just started connecting an MCP client yourself.</p>
<form method="post" action="/oauth/consent" class="actions">
  <input type="hidden" name="consent_id" value="{{.ConsentID}}">
  <button type="submit" name="action" value="deny">Deny</button>
  <button type="submit" name="action" value="approve" class="primary">Allow</button>
</form>
</main></body></html>{{end}}

{{define "error"}}{{template "head" .Title}}
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
</main></body></html>{{end}}
`))

type consentPage struct {
	ConsentID    string
	ClientName   string
	RedirectHost string
	Login        string
}

func renderConsent(w http.ResponseWriter, page consentPage) {
	render(w, http.StatusOK, "consent", page)
}

func renderError(w http.ResponseWriter, status int, title, message string) {
	render(w, status, "error", struct{ Title, Message string }{title, message})
}

func render(w http.ResponseWriter, status int, name string, data any) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// Stop the consent page being framed by another site and clicked through.
	h.Set("X-Frame-Options", "DENY")
	// No form-action directive: browsers apply it to the redirect that
	// follows the consent form, which goes to the MCP client's own URI.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("oauth: rendering %s page: %v", name, err)
	}
}
