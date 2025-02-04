package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/alyx/x/autocert"
	"golang.org/x/crypto/acme"
)

const stagingDirectoryURL = "https://acme-staging-v02.api.letsencrypt.org/directory"

// httpClient is used to do http request instead of the default http.DefaultClient.
// The OCSP server of Let's Encrypt certificates seems working improperly, gives
// `Unsolicited response received on idle HTTP channel starting with "HTTP/1.0 408 Request Time-out"`
// errors constantly after the service has been running for a long time.
// Using custom httpClient which disables Keep-Alive should fix this issue.
var httpClient *http.Client

func init() {
	mathrand.Seed(timeNow().UnixNano())
	httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				DualStack: true,
			}).DialContext,
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}

var ErrHostNotPermitted = errors.New("host not permitted")

func HostWhitelist(hosts ...string) autocert.HostPolicy {
	whitelist := autocert.HostWhitelist(hosts...)
	return func(ctx context.Context, host string) error {
		if whitelist(ctx, host) != nil {
			return ErrHostNotPermitted
		}
		return nil
	}
}

func RegexpWhitelist(patterns ...*regexp.Regexp) autocert.HostPolicy {
	return func(_ context.Context, host string) error {
		for _, p := range patterns {
			if p.MatchString(host) {
				return nil
			}
		}
		return ErrHostNotPermitted
	}
}

func EncodeRSAKey(w io.Writer, key *rsa.PrivateKey) error {
	b := x509.MarshalPKCS1PrivateKey(key)
	pb := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: b}
	return pem.Encode(w, pb)
}

func EncodeECDSAKey(w io.Writer, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	pb := &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	return pem.Encode(w, pb)
}

var manager *Manager

func GetManager() *Manager {
	if manager == nil {
		manager = &Manager{
			m: &autocert.Manager{
				Prompt:      autocert.AcceptTOS,
				Cache:       Cfg.Storage.Cache,
				RenewBefore: time.Duration(Cfg.LetsEncrypt.RenewBefore) * 24 * time.Hour,
				Client:      &acme.Client{DirectoryURL: Cfg.LetsEncrypt.DirectoryURL},
				Email:       Cfg.LetsEncrypt.Email,
				HostPolicy:  Cfg.LetsEncrypt.HostPolicy,
			},
			ForceRSA: Cfg.LetsEncrypt.ForceRSA,
		}
		if Cfg.LetsEncrypt.EABKID != "" && Cfg.LetsEncrypt.EABKey != "" {
			manager.m.ExternalAccountBinding = &acme.ExternalAccountBinding{KID: Cfg.LetsEncrypt.EABKID, Key: []byte(Cfg.LetsEncrypt.EABKey)}
		}
	}
	return manager
}

type Manager struct {
	m        *autocert.Manager
	ForceRSA bool
}

func (m *Manager) KeyName(domain string) string {
	if !m.ForceRSA {
		return domain
	}
	return domain + "+rsa"
}

func (m *Manager) OCSPKeyName(domain string) string {
	return fmt.Sprintf("autocert|%s", m.KeyName(domain))
}

func (m *Manager) helloInfo(domain string) *tls.ClientHelloInfo {
	helloInfo := &tls.ClientHelloInfo{ServerName: domain}
	if !m.ForceRSA {
		helloInfo.SignatureSchemes = append(helloInfo.SignatureSchemes,
			tls.ECDSAWithP256AndSHA256,
			tls.ECDSAWithP384AndSHA384,
			tls.ECDSAWithP521AndSHA512,
		)
		helloInfo.SupportedCurves = append(helloInfo.SupportedCurves, tls.CurveP256, tls.CurveP384, tls.CurveP521)
		helloInfo.CipherSuites = append(helloInfo.CipherSuites,
			tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		)
	}
	return helloInfo
}

func (m *Manager) GetAutocertCertificate(name string) (*tls.Certificate, error) {
	helloInfo := m.helloInfo(name)
	cert, err := m.m.GetCertificate(helloInfo)
	if err != nil {
		return nil, err
	}

	ocspKeyName := m.OCSPKeyName(name)
	OCSPManager.Watch(ocspKeyName, func() (*tls.Certificate, error) {
		helloInfo := m.helloInfo(name)
		return m.m.GetCertificate(helloInfo)
	})

	return cert, nil
}

func (m *Manager) GetAutocertALPN01Certificate(name string) (*tls.Certificate, error) {
	helloInfo := m.helloInfo(name)
	helloInfo.SupportedProtos = []string{acme.ALPNProto}
	return m.m.GetCertificate(helloInfo)
}

var rand63n = mathrand.Int63n

var testOCSPDidUpdateLoop = func(next time.Duration, err error) {}

var timeNow = time.Now
