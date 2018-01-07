package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	mathrand "math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gocraft/web"
	"github.com/jxskiss/glog"
	"github.com/jxskiss/ssl-cert-server/autocert"
	"golang.org/x/crypto/acme"
)

const VERSION = "0.1.1"

type StringArray []string

// Implement flag.Value interface.
func (v *StringArray) Set(s string) error {
	*v = append(*v, s)
	return nil
}

// Implement flag.Value interface.
func (v *StringArray) String() string {
	return strings.Join(*v, ",")
}

var (
	manager     autocert.Manager
	domainList  StringArray
	patternList StringArray

	showVersion = flag.Bool("version", false, "print version string and quit")
	listen      = flag.String("listen", "127.0.0.1:8999", "listen address, be sure DON't open to the world")
	staging     = flag.Bool("staging", false, "use Let's Encrypt staging directory (default false)")
	cacheDir    = flag.String("cache-dir", "./secret-dir", "which directory to cache certificates")
	before      = flag.Int("before", 30, "renew certificates before how many days")
	email       = flag.String("email", "", "contact email, if Let's Encrypt client's key is already registered, this is not used")
	forceRSA    = flag.Bool("force-rsa", false, "generate certificates with 2048-bit RSA keys (default false)")
)

func init() {
	flag.Var(&domainList, "domain", "allowed domain names (may be given multiple times)")
	flag.Var(&patternList, "pattern", "allowed domain regex pattern using POSIX ERE (egrep) syntax, (may be given multiple times, ignored when domain parameters supplied)")

	flag.Parse()

	var hostPolicy autocert.HostPolicy
	if len(domainList) > 0 {
		hostPolicy = autocert.HostWhitelist(domainList...)
	} else if len(patternList) > 0 {
		patterns := make([]*regexp.Regexp, len(patternList))
		for i, p := range patternList {
			r := regexp.MustCompilePOSIX(p)
			patterns[i] = r
		}
		hostPolicy = autocert.RegexpWhitelist(patterns...)
	} else {
		// allow any domain by default
		hostPolicy = func(ctx context.Context, host string) error {
			return nil
		}
	}

	var directoryUrl string
	if *staging {
		directoryUrl = "https://acme-staging.api.letsencrypt.org/directory"
	} else {
		directoryUrl = acme.LetsEncryptURL
	}

	manager = autocert.Manager{
		Prompt:      autocert.AcceptTOS,
		Cache:       autocert.DirCache(*cacheDir),
		RenewBefore: time.Duration(*before) * 24 * time.Hour,
		Client:      &acme.Client{DirectoryURL: directoryUrl},
		Email:       *email,
		ForceRSA:    *forceRSA,
		HostPolicy:  hostPolicy,
	}
}

type Context struct{}

func (c *Context) CertificateHandler(w web.ResponseWriter, r *web.Request) {
	domain := r.PathParams["domain"]
	cert, err := manager.GetCertificateByName(domain)
	if err != nil {
		if err == autocert.ErrNotPermitted {
			glog.Warningf("domain name not permitted: %s", domain)
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("Domain name not permitted."))
		} else {
			glog.Errorf("failed getting cert: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Error getting certificate."))
		}
		return
	}

	var (
		now        = time.Now()
		ttl        = cert.Leaf.NotAfter.Sub(now)
		ttlSeconds int
	)
	if ttl <= 0 {
		glog.Errorf("expired certificate for domain: %s", domain)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if ttl > 3600*time.Second {
		ttlSeconds = 3600
	} else {
		ttlSeconds = int(ttl.Seconds() * 0.8)
	}
	// add a little randomness to the TLL
	n := mathrand.Intn(100)
	if n < ttlSeconds {
		ttlSeconds -= n
	}

	var (
		certBuf    bytes.Buffer
		privKeyBuf bytes.Buffer
	)
	for _, b := range cert.Certificate {
		pb := &pem.Block{Type: "CERTIFICATE", Bytes: b}
		if err := pem.Encode(&certBuf, pb); err != nil {
			glog.Errorf("encoding certificate: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	switch key := cert.PrivateKey.(type) {
	case *rsa.PrivateKey:
		if err := autocert.EncodeRSAKey(&privKeyBuf, key); err != nil {
			glog.Errorf("encoding rsa private key: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	case *ecdsa.PrivateKey:
		if err := autocert.EncodeECDSAKey(&privKeyBuf, key); err != nil {
			glog.Errorf("encoding ecdsa key: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	default:
		glog.Errorf("unknown private key type")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(struct {
		Cert     string `json:"cert"`
		PKey     string `json:"pkey"`
		ExpireAt int64  `json:"expire_at"` // seconds since epoch
		TTL      int    `json:"ttl"`       // in seconds
	}{
		string(certBuf.Bytes()),
		string(privKeyBuf.Bytes()),
		cert.Leaf.NotAfter.Unix(),
		ttlSeconds,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func (c *Context) OCSPStaplingHandler(w web.ResponseWriter, r *web.Request) {
	domain := r.PathParams["domain"]
	response, nextUpdate, err := manager.GetOCSPStapling(domain)
	if err != nil {
		if err == autocert.ErrCacheMiss {
			w.WriteHeader(http.StatusNotFound)
		} else {
			glog.Errorf("ocsp stapling: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	var (
		now        = time.Now()
		ttl        = nextUpdate.Sub(now)
		ttlSeconds int
	)
	if ttl <= 0 {
		glog.Errorf("expired OCSP stapling for domain: %s", domain)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if ttl > 3600*time.Second {
		ttlSeconds = 3600
	} else {
		ttlSeconds = int(ttl.Seconds() * 0.8)
	}
	// add a little randomness to the TTL
	n := mathrand.Intn(100)
	if n < ttlSeconds {
		ttlSeconds -= n
	}

	w.Header().Set("Content-Type", "application/ocsp-response")
	w.Header().Set("X-Expire-At", fmt.Sprintf("%d", nextUpdate.Unix()))
	w.Header().Set("X-TTL", fmt.Sprintf("%d", ttlSeconds))
	w.Write(response)
}

func (c *Context) ChallengeHandler(w web.ResponseWriter, r *web.Request) {
	token := r.PathParams["token"]
	response, err := manager.GetHTTP01ChallengeResponse(token)
	if err != nil {
		if err == autocert.ChallengeNotFound {
			glog.Warningf("token not found: %s", token)
			w.WriteHeader(http.StatusNotFound)
		} else {
			glog.Errorf("challenge error: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	w.Write([]byte(response))
}

var stdout = log.New(os.Stdout, "", 0)

func accessLoggerMiddleware(rw web.ResponseWriter, req *web.Request, next web.NextMiddlewareFunc) {
	startTime := time.Now()

	next(rw, req)

	// Ammdd hhmmss Addr] Status Method URI Duration
	const LogFormat = "A%s %s] %d %s %s %s\n"

	duration := time.Since(startTime).Nanoseconds()
	var durationUnits string
	switch {
	case duration > 2000000:
		durationUnits = "ms"
		duration /= 1000000
	case duration > 1000:
		durationUnits = "μs"
		duration /= 1000
	default:
		durationUnits = "ns"
	}
	durationFormatted := fmt.Sprintf("%d%s", duration, durationUnits)

	logTime := time.Now().Format("0102 150405")
	stdout.Printf(LogFormat, logTime, req.RemoteAddr, rw.StatusCode(),
		req.Method, req.RequestURI, durationFormatted)
}

func main() {
	// flags are parsed in the `init()` function
	defer glog.Flush()

	if *showVersion {
		fmt.Printf("ssl-cert-server v%s\n", VERSION)
		return
	}

	router := web.New(Context{}).
		Middleware(accessLoggerMiddleware).
		Get("/cert/:domain:[^.]+\\..+", (*Context).CertificateHandler).
		Get("/ocsp/:domain:[^.]+\\..+", (*Context).OCSPStaplingHandler).
		Get("/.well-known/acme-challenge/:token", (*Context).ChallengeHandler)

	glog.Infof("Listening on http://%s", *listen)
	glog.Fatal(http.ListenAndServe(*listen, router))
}
