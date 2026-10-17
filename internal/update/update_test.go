package update

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseVersionAndCompare(t *testing.T) {
	tests := []struct {
		name   string
		left   string
		right  string
		result int
		valid  bool
	}{
		{name: "equal with v prefix", left: "v1.2.3", right: "1.2.3", result: 0, valid: true},
		{name: "two component release tags", left: "v0.6", right: "v0.7", result: -1, valid: true},
		{name: "patch update", left: "1.2.3", right: "1.2.4", result: -1, valid: true},
		{name: "major downgrade", left: "2.0.0", right: "1.9.9", result: 1, valid: true},
		{name: "release beats prerelease", left: "1.2.3-rc.1", right: "1.2.3", result: -1, valid: true},
		{name: "dev is not semver", left: "dev", right: "1.2.3", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, leftOK := parseVersion(test.left)
			right, rightOK := parseVersion(test.right)
			if leftOK != test.valid || !rightOK {
				t.Fatalf("parse validity = %v/%v, want %v/true", leftOK, rightOK, test.valid)
			}
			if test.valid && compareVersions(left, right) != test.result {
				t.Fatalf("compareVersions = %d, want %d", compareVersions(left, right), test.result)
			}
		})
	}
}

func TestTargetPlatform(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		hostOS   string
		hostArch string
		wantOS   string
		wantArch string
		wantErr  string
	}{
		{name: "host default", hostOS: "linux", hostArch: "amd64", wantOS: "linux", wantArch: "amd64"},
		{name: "architecture override", spec: "arm64", hostOS: "linux", hostArch: "amd64", wantOS: "linux", wantArch: "arm64"},
		{name: "x86 alias", spec: "x86_64", hostOS: "linux", hostArch: "arm64", wantOS: "linux", wantArch: "amd64"},
		{name: "full x86 alias", spec: "linux/x86_64", hostOS: "linux", hostArch: "arm64", wantOS: "linux", wantArch: "amd64"},
		{name: "full platform override", spec: "linux/aarch64", hostOS: "linux", hostArch: "amd64", wantOS: "linux", wantArch: "arm64"},
		{name: "different OS rejected", spec: "darwin_arm64", hostOS: "linux", hostArch: "amd64", wantErr: "does not match"},
		{name: "unsupported architecture", spec: "386", hostOS: "linux", hostArch: "amd64", wantErr: "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotOS, gotArch, err := targetPlatform(test.spec, test.hostOS, test.hostArch)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil || gotOS != test.wantOS || gotArch != test.wantArch {
				t.Fatalf("target = %s/%s, error = %v; want %s/%s", gotOS, gotArch, err, test.wantOS, test.wantArch)
			}
		})
	}
}

func TestFindAsset(t *testing.T) {
	assets := []asset{
		{Name: "q-agent_linux_amd64"},
		{Name: "q-agent_linux_arm64"},
	}
	if got, ok := findAsset(assets, "linux", "arm64"); !ok || got.Name != "q-agent_linux_arm64" {
		t.Fatalf("arm64 asset = %+v, %v", got, ok)
	}
	if _, ok := findAsset(assets, "darwin", "arm64"); ok {
		t.Fatal("found an asset that was not present")
	}
}

func TestRunDownloadsAndReplacesExecutable(t *testing.T) {
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "q-agent")
	if err := os.WriteFile(target, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}

	var downloads atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/latest":
			return fakeResponse(http.StatusOK, `{"tag_name":"v0.7","assets":[{"name":"q-agent_linux_amd64","browser_download_url":"https://api.test/download/linux"},{"name":"q-agent_linux_arm64","browser_download_url":"https://api.test/download/arm64"}]}`), nil
		case "/download/linux":
			downloads.Add(1)
			return fakeResponse(http.StatusOK, "new binary"), nil
		default:
			return fakeResponse(http.StatusNotFound, "not found"), nil
		}
	})}

	var output strings.Builder
	err := Run(context.Background(), nil, Options{
		CurrentVersion:       "v0.6",
		GOOS:                 "linux",
		GOARCH:               "amd64",
		ExecutablePath:       target,
		ReleaseURL:           "https://api.test/latest",
		Client:               client,
		Stdout:               &output,
		DownloadURLValidator: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new binary" || downloads.Load() != 1 {
		t.Fatalf("content = %q, downloads = %d", content, downloads.Load())
	}
	if !strings.Contains(output.String(), "from v0.6 to v0.7") || !strings.Contains(output.String(), target) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunSkipsDownloadWhenCurrentVersionMatches(t *testing.T) {
	var downloads atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/latest" {
			return fakeResponse(http.StatusOK, `{"tag_name":"1.1.0","assets":[]}`), nil
		}
		downloads.Add(1)
		return fakeResponse(http.StatusNotFound, "not found"), nil
	})}

	var output strings.Builder
	err := Run(context.Background(), nil, Options{
		CurrentVersion: "v1.1.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		ReleaseURL:     "https://api.test/latest",
		Client:         client,
		Stdout:         &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 0 || !strings.Contains(output.String(), "already up to date") {
		t.Fatalf("downloads = %d, output = %q", downloads.Load(), output.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func fakeResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestValidateDownloadURL(t *testing.T) {
	if err := validateDownloadURL("https://github.com/DemonGiggle/q-agent/releases/download/v1.2.3/q-agent_linux_amd64"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"http://github.com/DemonGiggle/q-agent/releases/download/v1.2.3/q-agent_linux_amd64",
		"https://example.com/DemonGiggle/q-agent/releases/download/v1.2.3/q-agent_linux_amd64",
		"https://github.com/other/repo/releases/download/v1.2.3/q-agent_linux_amd64",
	} {
		if err := validateDownloadURL(raw); err == nil {
			t.Fatalf("validateDownloadURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestRunPreservesSymlinkAndPermissions(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "installed"), filepath.Join(dir, "q-agent")
	if err := os.WriteFile(target, []byte("old binary"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("installed", link); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/latest" {
			return fakeResponse(200, `{"tag_name":"v1.2.3","assets":[{"name":"q-agent_linux_arm64","browser_download_url":"https://github.com/DemonGiggle/q-agent/releases/download/v1.2.3/q-agent_linux_arm64"}]}`), nil
		}
		return fakeResponse(200, "new binary"), nil
	})}
	if err := Run(context.Background(), []string{"--arch", "arm64"}, Options{
		CurrentVersion: "dev", GOOS: "linux", GOARCH: "amd64",
		ExecutablePath: link, ReleaseURL: "https://api.test/latest", Client: client,
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != "installed" {
		t.Fatalf("symlink = %q, error = %v", got, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new binary" {
		t.Fatalf("target = %q, error = %v", got, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("target permissions: %v, %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary files not cleaned up: %v, %v", entries, err)
	}
}

func TestRunFailuresLeaveExecutableIntact(t *testing.T) {
	const valid = `{"tag_name":"v1.2.3","assets":[{"name":"q-agent_linux_amd64","browser_download_url":"https://github.com/DemonGiggle/q-agent/releases/download/v1.2.3/q-agent_linux_amd64"}]}`
	for _, tc := range []struct {
		name, metadata, body, want     string
		metadataStatus, downloadStatus int
		broken                         bool
	}{
		{name: "no release", metadataStatus: 404, want: "no published"},
		{name: "rate limited", metadataStatus: 403, want: "GitHub returned"},
		{name: "invalid metadata", metadata: "{", want: "decode latest release"},
		{name: "missing tag", metadata: `{}`, want: "did not contain a tag"},
		{name: "invalid tag", metadata: `{"tag_name":"latest"}`, want: "unsupported version"},
		{name: "missing asset", metadata: `{"tag_name":"v1.2.3","assets":[]}`, want: "no asset"},
		{name: "missing URL", metadata: `{"tag_name":"v1.2.3","assets":[{"name":"q-agent_linux_amd64"}]}`, want: "no download URL"},
		{name: "wrong repository", metadata: strings.ReplaceAll(valid, "/q-agent/releases/", "/qcode/releases/"), want: "invalid download URL"},
		{name: "download failure", metadata: valid, downloadStatus: 500, want: "download update"},
		{name: "empty download", metadata: valid, want: "empty"},
		{name: "interrupted download", metadata: valid, broken: true, want: "write update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "q-agent")
			if err := os.WriteFile(target, []byte("old binary"), 0755); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/latest" {
					status := tc.metadataStatus
					if status == 0 {
						status = 200
					}
					return fakeResponse(status, tc.metadata), nil
				}
				status := tc.downloadStatus
				if status == 0 {
					status = 200
				}
				response := fakeResponse(status, tc.body)
				if tc.broken {
					response.Body = io.NopCloser(&failedDownload{})
				}
				return response, nil
			})}
			err := Run(context.Background(), nil, Options{
				CurrentVersion: "v1.0.0", GOOS: "linux", GOARCH: "amd64",
				ExecutablePath: target, ReleaseURL: "https://api.test/latest", Client: client,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "old binary" {
				t.Fatalf("target = %q, error = %v", got, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary files leaked: %v, %v", entries, err)
			}
		})
	}
}

type failedDownload struct{ sent bool }

func (r *failedDownload) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial download"), nil
}

func TestRunRejectsArgsBeforeNetwork(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid arguments caused network request")
		return nil, io.ErrUnexpectedEOF
	})}
	for _, args := range [][]string{{"--help"}, {"--arch", "darwin/arm64"}, {"--arch", "386"}, {"extra"}, {"--bad"}} {
		if err := Run(context.Background(), args, Options{GOOS: "linux", GOARCH: "amd64", Client: client}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRunDoesNotDowngrade(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/latest" {
			t.Fatal("unexpected download")
		}
		return fakeResponse(200, `{"tag_name":"v1.2.3"}`), nil
	})}
	var output strings.Builder
	if err := Run(context.Background(), nil, Options{
		CurrentVersion: "v2.0.0", GOOS: "linux", GOARCH: "amd64",
		ReleaseURL: "https://api.test/latest", Client: client, Stdout: &output,
	}); err != nil || !strings.Contains(output.String(), "newer than") {
		t.Fatalf("error = %v, output = %s", err, &output)
	}
}
