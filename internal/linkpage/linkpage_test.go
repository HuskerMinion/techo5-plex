package linkpage

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

type fakeLinker struct {
	st      plextv.Status
	started int
}

func (f *fakeLinker) Status() plextv.Status { return f.st }
func (f *fakeLinker) Start() (string, error) {
	f.started++
	f.st = plextv.Status{Waiting: true, Code: "AB12"}
	return "AB12", nil
}
func (f *fakeLinker) Cancel()       { f.st = plextv.Status{} }
func (f *fakeLinker) Unlink() error { f.st = plextv.Status{}; return nil }

func do(h http.Handler, method, target, remote, host string, form url.Values) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.RemoteAddr, r.Host = remote, host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The page shows the code to type, takes its buttons only from itself, and only from the home
// network at the device's address.
func TestThePage(t *testing.T) {
	fl := &fakeLinker{}
	pg := New(fl, func() string { return "Kitchen" }, func() string { return "" }, func() (string, string) { return "", "" })
	h := pg.Handler()
	const lan, host = "192.168.1.5:5000", "192.168.1.9:32501"

	if w := do(h, "GET", "/", "8.8.8.8:1", host, nil); w.Code != http.StatusForbidden {
		t.Errorf("from the internet: %d", w.Code)
	}
	if w := do(h, "GET", "/", lan, "rebound.example:32501", nil); w.Code != http.StatusForbidden {
		t.Errorf("a name in Host: %d", w.Code)
	}
	if w := do(h, "POST", "/link", lan, host, url.Values{"form": {"wrong"}}); w.Code != http.StatusForbidden || fl.started != 0 {
		t.Errorf("a forged form: %d, started %d", w.Code, fl.started)
	}
	if w := do(h, "POST", "/link", lan, host, url.Values{"form": {pg.form}}); w.Code != http.StatusSeeOther || fl.started != 1 {
		t.Errorf("the Link button: %d, started %d", w.Code, fl.started)
	}
	w := do(h, "GET", "/", lan, host, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AB12") || !strings.Contains(w.Body.String(), plextv.LinkURL) {
		t.Errorf("the code is not shown: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/status.json", lan, host, nil); !strings.Contains(w.Body.String(), `"code":"AB12"`) {
		t.Errorf("status.json %s", w.Body.String())
	}
}

// A renderer that is not answering is said on the page.
func TestTheRendererProblemIsShown(t *testing.T) {
	pg := New(&fakeLinker{}, func() string { return "K" }, func() string { return "turn on DLNA in TECHO5" }, nil)
	w := do(pg.Handler(), "GET", "/", "192.168.1.5:1", "192.168.1.9", nil)
	if !strings.Contains(w.Body.String(), "turn on DLNA in TECHO5") {
		t.Error(w.Body.String())
	}
}
