package forkupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

func pinnedGET(ctx context.Context, client *http.Client, origin, endpoint string) (*http.Response, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if base.Scheme != "https" || base.Host == "" || base.User != nil || (base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("update URL is outside pinned HTTPS origin")
	}
	if client == nil {
		return nil, errors.New("HTTP client is required")
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("private update endpoints must not redirect")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := clone.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("update endpoint returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func FetchCatalog(ctx context.Context, client *http.Client, policy Policy, endpoint string, maxBytes int64) (VerifiedCatalog, []byte, error) {
	var empty VerifiedCatalog
	if maxBytes <= 0 || maxBytes == int64(^uint64(0)>>1) {
		return empty, nil, errors.New("positive catalog byte limit is required")
	}
	response, err := pinnedGET(ctx, client, policy.Origin, endpoint)
	if err != nil {
		return empty, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return empty, nil, err
	}
	if int64(len(data)) > maxBytes {
		return empty, nil, errors.New("catalog exceeds configured byte limit")
	}
	candidate, err := VerifyCatalog(data, policy)
	return candidate, data, err
}

// DownloadArtifact only exposes a verified immutable cache entry. Interrupted
// transfers leave no final package; existing corrupt entries fail explicitly.
func DownloadArtifact(ctx context.Context, client *http.Client, policy Policy, artifact Artifact, cache string) (string, error) {
	if !filepath.IsAbs(cache) || !digestPattern.MatchString(artifact.SHA256) {
		return "", errors.New("absolute cache and signed digest required")
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		return "", err
	}
	target := filepath.Join(cache, artifact.SHA256)
	unlock, err := lockJournal(target + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Lstat(target); err == nil {
		f, err := os.Open(target)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if err := VerifyArtifact(f, artifact); err != nil {
			return "", fmt.Errorf("corrupt cached artifact: %w", err)
		}
		return target, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	response, err := pinnedGET(ctx, client, policy.Origin, artifact.URL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	f, err := os.CreateTemp(cache, ".download-*")
	if err != nil {
		return "", err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	defer f.Close()
	if artifact.Bytes <= 0 || artifact.Bytes == int64(^uint64(0)>>1) {
		return "", errors.New("invalid signed artifact length")
	}
	if _, err := io.Copy(f, io.LimitReader(response.Body, artifact.Bytes+1)); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err := VerifyArtifact(f, artifact); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", err
	}
	if err := syncDirectory(cache); err != nil {
		return "", err
	}
	return target, nil
}
