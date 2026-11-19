package forkupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type WorkerConfig struct {
	NativeArtifactAuth bool          `json:"native_artifact_auth"`
	NativeToken        string        `json:"-"`
	AutomaticChecks    bool          `json:"automatic_checks"`
	Host               string        `json:"host"`
	StateDir           string        `json:"state_dir"`
	LockDir            string        `json:"lock_dir"`
	CacheDir           string        `json:"cache_dir"`
	ControlURL         string        `json:"control_url"`
	RuntimeID          string        `json:"runtime_id"`
	TokenFile          string        `json:"token_file"`
	PublicKey          string        `json:"public_key"`
	Channel            string        `json:"channel"`
	Origin             string        `json:"origin"`
	HTTPTimeout        string        `json:"http_timeout"`
	CAFile             string        `json:"ca_file,omitempty"`
	Adapter            AdapterPolicy `json:"adapter"`
}

func LoadWorker(path string) (WorkerConfig, error) {
	var config WorkerConfig
	if err := verifyLocalFile(path, "", true); err != nil {
		return config, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		err = decodeStrict(data, &config)
	}
	return config, err
}
func (c WorkerConfig) Tick(ctx context.Context) error {
	timeout, err := time.ParseDuration(c.HTTPTimeout)
	if err != nil || timeout <= 0 {
		return errors.New("explicit positive HTTP timeout required")
	}
	if !idPattern.MatchString(c.Host) || !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.LockDir) || !filepath.IsAbs(c.CacheDir) {
		return errors.New("invalid host storage policy")
	}
	token := []byte(c.NativeToken)
	if c.TokenFile != "" {
		if err := verifyLocalFile(c.TokenFile, "", true); err != nil {
			return err
		}
		token, err = os.ReadFile(c.TokenFile)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(string(token)) == "" {
		return errors.New("native profile credentials are unavailable")
	}
	key, err := base64.StdEncoding.DecodeString(c.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("invalid pinned update key")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.CAFile != "" {
		if err := verifyLocalFile(c.CAFile, "", false); err != nil {
			return err
		}
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("invalid private channel CA")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}
	artifactClient := client
	if c.NativeArtifactAuth {
		origin, err := url.Parse(c.Origin)
		if err != nil {
			return err
		}
		control, err := url.Parse(c.ControlURL)
		if err != nil {
			return err
		}
		if origin.Scheme != control.Scheme || origin.Host != control.Host {
			return errors.New("artifact authentication requires the exact native profile origin")
		}
		artifactClient = &http.Client{Transport: nativeArtifactTransport{base: transport, origin: c.Origin, token: strings.TrimSpace(string(token))}, Timeout: timeout}
	}
	automatic := c.AutomaticChecks
	checkRequest := filepath.Join(c.StateDir, "check-request.json")
	if info, err := os.Lstat(checkRequest); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("unsafe manual check request")
		}
		automatic = true
	} else if !os.IsNotExist(err) {
		return err
	}
	consumer := Consumer{AutoPrepare: &automatic, Client: ControlClient{BaseURL: c.ControlURL, RuntimeID: c.RuntimeID, RefreshCatalog: &automatic, Token: strings.TrimSpace(string(token)), HTTP: client}, Policy: Policy{RequireSource: true, PublicKey: key, Channel: c.Channel, Origin: c.Origin}, Host: c.Host, StateDir: c.StateDir, LockDir: c.LockDir, CacheDir: c.CacheDir, Artifacts: artifactClient, Driver: c.Adapter.Factory}
	err = consumer.Tick(ctx)
	if err == nil && automatic {
		if removeErr := os.Remove(checkRequest); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
	}
	return err
}

type nativeArtifactTransport struct {
	base          http.RoundTripper
	origin, token string
}

func (t nativeArtifactTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	origin, err := url.Parse(t.origin)
	if err != nil {
		return nil, err
	}
	if r.URL.Scheme != origin.Scheme || r.URL.Host != origin.Host {
		return nil, errors.New("artifact credential origin mismatch")
	}
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
