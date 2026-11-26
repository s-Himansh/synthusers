// Package brokensite is a deliberately broken demo shop for exercising the
// synthetic users end to end. It serves HTTP on one port and HTTPS (with a
// self-signed certificate) on another, and every problem family of the
// library is deliberately present somewhere:
//
//	missing images / scripts / CSS-referenced assets   -> broken_asset
//	internal + external dead links                     -> broken_link
//	http:// asset on the https site                    -> mixed_content
//	a page that returns HTTP 500                       -> http_status
//	a 6 second endpoint                                -> slow_response
//	a "200 OK" page that is actually a not-found page  -> soft_404
//	short-lived TLS certificate (default 10 days)      -> ssl_expiry
//	a page that throws and console.errors on load      -> js_error
//	a working login form                               -> journeys
package brokensite

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Options configures the demo site.
type Options struct {
	// HTTPOrigin is the http:// origin of the plain-HTTP server; pages
	// embed one asset from it so https pages contain real mixed content.
	HTTPOrigin string
}

// NewHandler returns the demo shop handler.
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()

	const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	const gif1x1 = "R0lGODlhAQABAIAAAP///wAAACH5BAEAAAAALAAAAAABAAEAAAICRAEAOw=="

	serveImage := func(w http.ResponseWriter, data string) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(mustDecode(data))
	}

	mux.HandleFunc("GET /static/ok-bg.png", func(w http.ResponseWriter, r *http.Request) { serveImage(w, png1x1) })
	mux.HandleFunc("GET /static/pixel.gif", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Write(mustDecode(gif1x1))
	})
	mux.HandleFunc("GET /static/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, `body { background: url(/static/ok-bg.png); }
.cart { background: url(/static/nope-bg.png); } /* referenced but missing */
`)
	})

	sessions := &sessionStore{}
	nav := func() string {
		var b strings.Builder
		b.WriteString(`<nav style="display:flex;gap:12px">`)
		for _, l := range []struct{ href, label string }{
			{"/", "Home"}, {"/products", "Products"}, {"/login", "Login"},
			{"/slow", "Slow page"}, {"/broken-500", "Broken page"}, {"/linkfarm", "Directory"}, {"/js-error", "Widgets"},
		} {
			fmt.Fprintf(&b, `<a href="%s">%s</a>`, l.href, l.label)
		}
		b.WriteString(`</nav>`)
		return b.String()
	}
	page := func(title, body string, extraHead string) string {
		return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%s</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="/static/style.css">%s</head><body>
<img src="/static/missing-logo.png" alt="logo" width="80" height="20" />
<script src="/static/app.js"></script>
%s
%s
<hr /><small>Demo Shop — deliberately broken site for synthusers. <a href="/api/health">API health</a></small>
</body></html>`, title, extraHead, nav(), body)
	}

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		mixed := ""
		if opts.HTTPOrigin != "" {
			mixed = fmt.Sprintf(`<img src="%s/static/pixel.gif" width="1" height="1" alt="" />`, opts.HTTPOrigin)
		}
		body := `<h1>Demo Shop — Home</h1>
<p>Welcome to the demo shop. Browse the <a href="/products">products</a>, check the <a href="/linkfarm">directory</a>, or <a href="/login">sign in</a>.</p>
<p>More: <a href="/slow">slow page</a>, <a href="/broken-500">broken page</a>, <a href="/js-error">widgets</a>.</p>` + mixed
		w.Write([]byte(page("Demo Shop — Home", body, "")))
	})

	mux.HandleFunc("GET /products", func(w http.ResponseWriter, r *http.Request) {
		body := `<h1>Products</h1>
<ul>
<li><a href="/products/1">Mechanical keyboard</a> — In stock</li>
<li><a href="/products/2">USB-C dock</a> — In stock</li>
<li><a href="/products/3">Ergonomic mouse</a> — In stock</li>
</ul>
<p>Also see our <a href="/gone">discontinued items</a> and <a href="http://127.0.0.1:9/">partner site</a>.</p>`
		w.Write([]byte(page("Products — Demo Shop", body, "")))
	})

	mux.HandleFunc("GET /products/{n}", func(w http.ResponseWriter, r *http.Request) {
		n := r.PathValue("n")
		switch n {
		case "1":
			body := `<h1>Mechanical keyboard</h1><p>In stock — 42 units. <img src="/static/broken.jpg" alt="product photo" /></p><p><a href="/products">Back to products</a></p>`
			w.Write([]byte(page("Mechanical keyboard — Demo Shop", body, "")))
		case "2":
			body := `<h1>USB-C dock</h1><p>In stock — 7 units.</p><p><a href="/products">Back to products</a></p>`
			w.Write([]byte(page("USB-C dock — Demo Shop", body, "")))
		default:
			// Soft 404: HTTP 200 with a "not found" face.
			body := `<h1>Not Found</h1><p>The page you requested does not exist. <a href="/products">Back to products</a></p>`
			w.Write([]byte(page("Page not found - Demo Shop", body, "")))
		}
	})

	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(6 * time.Second)
		w.Write([]byte(page("Slow page — Demo Shop", `<h1>Slow page</h1><p>Sorry, this page took 6 seconds.</p>`, "")))
	})

	mux.HandleFunc("GET /broken-500", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error: price index corrupted", http.StatusInternalServerError)
	})

	mux.HandleFunc("GET /gone", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	mux.HandleFunc("GET /linkfarm", func(w http.ResponseWriter, r *http.Request) {
		body := `<h1>Directory</h1>
<ul>
<li><a href="/">Home</a></li>
<li><a href="/products">Products</a></li>
<li><a href="/js-error">Widgets</a></li>
<li><a href="/gone">Discontinued items</a></li>
<li><a href="/products/3">Ergonomic mouse</a></li>
<li><a href="http://127.0.0.1:9/">Partner site</a></li>
</ul>`
		w.Write([]byte(page("Directory — Demo Shop", body, "")))
	})

	mux.HandleFunc("GET /js-error", func(w http.ResponseWriter, r *http.Request) {
		body := `<h1>Widgets dashboard</h1><p>This dashboard loads a widget framework that is having a bad day.</p>
<script>window.addEventListener('load', function () {
  console.error('widget framework: init failed for widget "cart-preview"');
  var w = {}; w.cartPreview.render();
});</script>`
		w.Write([]byte(page("Widgets — Demo Shop", body, "")))
	})

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		body := `<h1>Sign in</h1>` + loginForm("")
		w.Write([]byte(page("Sign in — Demo Shop", body, "")))
	})

	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		user := r.FormValue("username")
		pass := r.FormValue("password")
		if user == "demo" && pass == "hunter2" {
			sessions.create(w)
			http.Redirect(w, r, "/account", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(page("Sign in — Demo Shop", "<h1>Sign in</h1>"+loginForm("Invalid credentials — try demo / hunter2."), "")))
	})

	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		if !sessions.valid(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		body := `<h1>Welcome back, demo user!</h1><p>Your orders are on time. <a href="/logout">Sign out</a></p>`
		w.Write([]byte(page("Account — Demo Shop", body, "")))
	})

	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		sessions.destroy(w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	return mux
}

func loginForm(errMsg string) string {
	e := ""
	if errMsg != "" {
		e = `<p style="color:red">` + errMsg + `</p>`
	}
	return e + `<form action="/login" method="post">
<input type="hidden" name="csrf" value="demo-csrf-token" />
<label>Username <input type="text" name="username" /></label><br />
<label>Password <input type="password" name="password" /></label><br />
<button type="submit">Sign in</button>
</form>`
}

type sessionStore struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (s *sessionStore) create(w http.ResponseWriter) {
	id := fmt.Sprintf("sid-%d", time.Now().UnixNano())
	s.mu.Lock()
	if s.ids == nil {
		s.ids = map[string]bool{}
	}
	s.ids[id] = true
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: id, Path: "/", HttpOnly: true})
}

func (s *sessionStore) valid(r *http.Request) bool {
	c, err := r.Cookie("sid")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[c.Value]
}

func (s *sessionStore) destroy(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1})
}

// SelfSignedCert generates a self-signed TLS certificate valid for the
// given number of days, covering localhost and 127.0.0.1.
func SelfSignedCert(days int) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	if days <= 0 {
		days = 10
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "brokensite.demo"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Duration(days) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key, // must implement crypto.Signer for the TLS stack
	}, nil
}

func mustDecode(b64 string) []byte {
	out, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	return out
}

// Main is the real main() of the example command.
// Main is the entry point used by cmd/brokensite.
func Main() {
	httpAddr := flag.String("http", ":6060", "plain HTTP listen address")
	httpsAddr := flag.String("https", ":6161", "HTTPS listen address (self-signed)")
	certDays := flag.Int("cert-days", 10, "validity of the self-signed certificate in days (below ssl_warn_days to demo ssl_expiry)")
	flag.Parse()

	origin := ""
	if *httpAddr != "" {
		origin = "http://127.0.0.1" + *httpAddr
	}

	cert, err := SelfSignedCert(*certDays)
	if err != nil {
		log.Fatalf("generate cert: %v", err)
	}
	httpsSrv := &http.Server{
		Addr:    *httpsAddr,
		Handler: NewHandler(Options{HTTPOrigin: origin}),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() {
		log.Printf("brokensite https listening on %s (self-signed, %d days)", *httpsAddr, *certDays)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("https: %v", err)
		}
	}()
	log.Printf("brokensite http listening on %s", *httpAddr)
	if err := http.ListenAndServe(*httpAddr, NewHandler(Options{})); err != nil {
		log.Fatalf("http: %v", err)
	}
}
