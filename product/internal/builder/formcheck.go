package builder

// The post-deploy review's checks are GET-shaped: the render test, the audit
// crawl and the screenshots all prove pages *display*. None of them prove the
// site's primary action *runs* — and the failure they can't see is exactly the
// one that shipped on SEO Probe: a POST handler re-rendering a template whose
// fields no longer match the result type. Behind {{with .Data}} the GET renders
// clean and only the submit explodes. So the review also submits the site's
// primary public form with clearly-marked test data and requires the response
// not to crash. A validation rejection (4xx) is a PASS — the path executed.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// formCheckTimeout bounds each request of the form check (fetch, then submit).
const formCheckTimeout = 25 * time.Second

// auditPrimaryForm fetches pageURL, finds its first public POST form, submits
// it with test data, and reports a Finding when the submit crashes (>=500 or a
// template error). The note is a human log line emitted either way.
func auditPrimaryForm(ctx context.Context, pageURL string) (*Finding, string) {
	client := &http.Client{Timeout: formCheckTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, "Form check skipped: " + err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "Form check skipped: could not fetch the page."
	}
	doc, err := html.Parse(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, "Form check skipped: could not parse the page."
	}

	form := findPrimaryForm(doc)
	if form == nil {
		return nil, "Form check: no public form on the landing page — skipped."
	}
	action := resolveAction(pageURL, form.action)

	post, err := http.NewRequestWithContext(ctx, http.MethodPost, action,
		strings.NewReader(form.values.Encode()))
	if err != nil {
		return nil, "Form check skipped: " + err.Error()
	}
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	presp, err := client.Do(post)
	if err != nil {
		return nil, fmt.Sprintf("Form check: POST %s did not answer (%v).", form.action, err)
	}
	body, _ := io.ReadAll(io.LimitReader(presp.Body, 128<<10))
	presp.Body.Close()

	// render() surfaces template failures as loud 500s with this marker, so a
	// torn template/struct pair is caught even if a proxy rewrites the status.
	if presp.StatusCode >= 500 || strings.Contains(string(body), "template error in") {
		return &Finding{
			Antipattern: "broken-primary-form",
			Name:        "Primary form crashes on submit",
			Severity:    "error",
			Description: fmt.Sprintf("POST %s with test data returned %d — the page's main "+
				"call-to-action is broken for every visitor. Fix the handler/template pair and "+
				"add a test that POSTs this form and asserts a clean render (the starter's page "+
				"test only covers GETs; see AGENTS.md).", form.action, presp.StatusCode),
			Snippet: firstLine(string(body)),
		}, fmt.Sprintf("Form check: POST %s → %d ✗ the primary form crashes", form.action, presp.StatusCode)
	}
	return nil, fmt.Sprintf("Form check: POST %s → %d ✓", form.action, presp.StatusCode)
}

// parsedForm is one submittable form: its action and prefilled test values.
type parsedForm struct {
	action string
	values url.Values
}

// findPrimaryForm returns the first form worth exercising: method POST, not an
// auth form (no password field, action not an auth path). Document order means
// the hero/primary CTA wins.
func findPrimaryForm(doc *html.Node) *parsedForm {
	for _, f := range collectForms(doc) {
		if f == nil {
			continue
		}
		return f
	}
	return nil
}

func collectForms(n *html.Node) []*parsedForm {
	var out []*parsedForm
	if n.Type == html.ElementNode && n.Data == "form" {
		out = append(out, parseForm(n))
		return out // forms don't nest
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, collectForms(c)...)
	}
	return out
}

// parseForm turns a <form> node into a submittable parsedForm, or nil when the
// form shouldn't be exercised (GET form, auth form).
func parseForm(n *html.Node) *parsedForm {
	if !strings.EqualFold(attr(n, "method"), "post") {
		return nil
	}
	action := attr(n, "action")
	low := strings.ToLower(action)
	for _, auth := range []string{"login", "signup", "signin", "register", "logout", "password"} {
		if strings.Contains(low, auth) {
			return nil
		}
	}
	f := &parsedForm{action: action, values: url.Values{}}
	seenRadio := map[string]bool{}
	var walk func(*html.Node) bool // false → abandon this form (auth)
	walk = func(c *html.Node) bool {
		if c.Type == html.ElementNode {
			name := attr(c, "name")
			switch c.Data {
			case "input":
				typ := strings.ToLower(attr(c, "type"))
				switch typ {
				case "password":
					return false // an auth form after all
				case "submit", "button", "reset", "image", "file":
				case "hidden":
					if name != "" {
						f.values.Set(name, attr(c, "value")) // e.g. tokens — keep as-is
					}
				case "checkbox":
					if name != "" {
						f.values.Set(name, orDefault(attr(c, "value"), "on"))
					}
				case "radio":
					if name != "" && !seenRadio[name] {
						seenRadio[name] = true
						f.values.Set(name, orDefault(attr(c, "value"), "on"))
					}
				default:
					if name != "" {
						f.values.Set(name, sampleValue(typ, name))
					}
				}
			case "textarea":
				if name != "" {
					f.values.Set(name, sampleValue("textarea", name))
				}
			case "select":
				if name != "" {
					f.values.Set(name, firstOption(c))
				}
			}
		}
		for gc := c.FirstChild; gc != nil; gc = gc.NextSibling {
			if !walk(gc) {
				return false
			}
		}
		return true
	}
	if !walk(n) {
		return nil
	}
	return f
}

// sampleValue is the test data for one field, picked from its type/name. The
// text is deliberately self-identifying so a site owner who finds it stored
// knows it was Forge's automated check, not a real visitor.
func sampleValue(typ, name string) string {
	n := strings.ToLower(name)
	switch {
	case typ == "email" || strings.Contains(n, "mail"):
		return "forge-check@example.com"
	case typ == "tel" || strings.Contains(n, "phone") || strings.Contains(n, "tel"):
		return "+46 70 123 45 67"
	case typ == "url" || strings.Contains(n, "url") || strings.Contains(n, "website"):
		return "https://example.com"
	case typ == "number" || strings.Contains(n, "count") || strings.Contains(n, "qty"):
		return "1"
	case typ == "date":
		return "2030-01-15"
	case typ == "time":
		return "12:00"
	case typ == "textarea":
		return "Automatic check from Transcend Forge — safe to delete."
	default:
		return "Forge check"
	}
}

func firstOption(sel *html.Node) string {
	for c := sel.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "option" {
			if v := attr(c, "value"); v != "" {
				return v
			}
			if c.FirstChild != nil && c.FirstChild.Type == html.TextNode {
				return strings.TrimSpace(c.FirstChild.Data)
			}
		}
	}
	return ""
}

// resolveAction resolves a form action against the page it was found on.
func resolveAction(pageURL, action string) string {
	if action == "" {
		return pageURL
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return action
	}
	ref, err := url.Parse(action)
	if err != nil {
		return action
	}
	return base.ResolveReference(ref).String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
