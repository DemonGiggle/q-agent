// Package update implements q-agent's self-update command.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	latestReleaseURL   = "https://api.github.com/repos/DemonGiggle/q-agent/releases/latest"
	maxReleaseMetadata = 1 << 20
	maxUpdateSize      = 128 << 20
)

// Options controls an update run. The platform and executable fields are
// injectable so the update workflow can be tested without changing the host.
type Options struct {
	CurrentVersion string
	GOOS           string
	GOARCH         string
	ExecutablePath string
	ReleaseURL     string
	Client         *http.Client
	Stdout         io.Writer
	Stderr         io.Writer
	// Service maintenance requires a native static executable before replacing it.
	ValidateExecutable bool

	// DownloadURLValidator overrides the production GitHub URL validation for
	// tests. A nil validator requires an HTTPS github.com release URL.
	DownloadURLValidator func(string) error
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type updateArgs struct {
	arch string
}

// Run executes q-agent update with the supplied arguments.
func Run(ctx context.Context, arguments []string, options Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	if options.ReleaseURL == "" {
		options.ReleaseURL = latestReleaseURL
	}
	if options.Client == nil {
		options.Client = &http.Client{Timeout: 30 * time.Second}
	}

	parsed, err := parseArgs(arguments, options.Stderr)
	if err != nil {
		return err
	}

	targetOS, targetArch, err := targetPlatform(parsed.arch, options.GOOS, options.GOARCH)
	if err != nil {
		return err
	}

	latest, err := fetchLatestRelease(ctx, options.Client, options.ReleaseURL)
	if err != nil {
		return err
	}
	latestVersion, ok := parseVersion(latest.TagName)
	if !ok {
		return fmt.Errorf("latest release has unsupported version tag %q", latest.TagName)
	}

	if current, ok := parseVersion(options.CurrentVersion); ok {
		switch compareVersions(current, latestVersion) {
		case 0:
			fmt.Fprintf(options.Stdout, "q-agent is already up to date (%s)\n", latest.TagName)
			return nil
		case 1:
			fmt.Fprintf(options.Stdout, "q-agent %s is newer than the latest release %s\n", options.CurrentVersion, latest.TagName)
			return nil
		}
	}

	targetAsset, ok := findReleaseAsset(latest.Assets, latest.TagName, targetOS, targetArch)
	if !ok {
		return fmt.Errorf("release %s has no asset for %s/%s", latest.TagName, targetOS, targetArch)
	}
	if targetAsset.BrowserDownloadURL == "" {
		return fmt.Errorf("release asset %q has no download URL", targetAsset.Name)
	}
	validator := options.DownloadURLValidator
	if validator == nil {
		validator = validateDownloadURL
	}
	if err := validator(targetAsset.BrowserDownloadURL); err != nil {
		return fmt.Errorf("invalid download URL for asset %q: %w", targetAsset.Name, err)
	}

	executable, target, mode, err := executableTarget(options.ExecutablePath)
	if err != nil {
		return err
	}
	temporary, err := downloadAsset(ctx, options.Client, targetAsset.BrowserDownloadURL, filepath.Dir(target), mode, validator)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if strings.HasSuffix(targetAsset.Name, ".tar.gz") {
		extracted, err := unpackArchive(temporary, mode)
		if err != nil {
			return err
		}
		defer os.Remove(extracted)
		temporary = extracted
	}
	if options.ValidateExecutable {
		if err := validateELF(temporary, targetArch); err != nil {
			return err
		}
	}

	if err := installUnix(temporary, target, mode); err != nil {
		return err
	}
	fmt.Fprintf(options.Stdout, "updated q-agent at %s from %s to %s\n", executable, options.CurrentVersion, latest.TagName)
	return nil
}

func parseArgs(arguments []string, stderr io.Writer) (updateArgs, error) {
	var result updateArgs
	flags := flag.NewFlagSet("q-agent update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&result.arch, "arch", "", "target architecture (default: current architecture; e.g. arm64)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: q-agent update [--arch architecture]")
		fmt.Fprintln(stderr, "\nDownload and install the latest release for the current operating system.")
		fmt.Fprintln(stderr, "Supported targets: linux/amd64 and linux/arm64.")
	}
	if err := flags.Parse(arguments); err != nil {
		return updateArgs{}, err
	}
	if flags.NArg() != 0 {
		return updateArgs{}, fmt.Errorf("q-agent update does not accept positional arguments")
	}
	return result, nil
}

func targetPlatform(spec, hostOS, hostArch string) (string, string, error) {
	targetOS, targetArch := strings.ToLower(strings.TrimSpace(hostOS)), normalizeArch(hostArch)
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec != "" {
		spec = strings.NewReplacer("/", "_", "-", "_").Replace(spec)
		arch := normalizeArch(spec)
		if arch == "amd64" || arch == "arm64" {
			targetArch = arch
		} else if osSpec, archSpec, ok := strings.Cut(spec, "_"); ok {
			targetOS, targetArch = osSpec, normalizeArch(archSpec)
		} else {
			targetArch = arch
		}
	}
	if targetOS != strings.ToLower(hostOS) {
		return "", "", fmt.Errorf("target operating system %q does not match the host operating system %q", targetOS, hostOS)
	}
	if !supportedTarget(targetOS, targetArch) {
		return "", "", fmt.Errorf("unsupported update target %s/%s", targetOS, targetArch)
	}
	return targetOS, targetArch, nil
}

func normalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

func supportedTarget(goos, goarch string) bool {
	return goos == "linux" && (goarch == "amd64" || goarch == "arm64")
}

func fetchLatestRelease(ctx context.Context, client *http.Client, endpoint string) (release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release{}, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "q-agent-update")
	response, err := client.Do(request)
	if err != nil {
		return release{}, fmt.Errorf("check latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return release{}, errors.New("no published q-agent release found; retry after a release is published")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return release{}, fmt.Errorf("check latest release: GitHub returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	var result release
	if err := json.NewDecoder(io.LimitReader(response.Body, maxReleaseMetadata)).Decode(&result); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	if strings.TrimSpace(result.TagName) == "" {
		return release{}, errors.New("latest release response did not contain a tag")
	}
	return result, nil
}

func findAsset(assets []asset, goos, goarch string) (asset, bool) {
	wanted := fmt.Sprintf("q-agent_%s_%s", goos, goarch)
	for _, candidate := range assets {
		if candidate.Name == wanted {
			return candidate, true
		}
	}
	return asset{}, false
}

func findReleaseAsset(assets []asset, tag, goos, goarch string) (asset, bool) {
	if raw, ok := findAsset(assets, goos, goarch); ok {
		return raw, true
	}
	wanted := fmt.Sprintf("q-agent_%s_%s_%s.tar.gz", tag, goos, goarch)
	for _, candidate := range assets {
		if candidate.Name == wanted {
			return candidate, true
		}
	}
	return asset{}, false
}

// Only the top-level regular q-agent entry is copied, never arbitrary paths,
// permissions, links, credentials or configuration from a release archive.
func unpackArchive(path string, mode os.FileMode) (result string, err error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	z, err := gzip.NewReader(in)
	if err != nil {
		return "", fmt.Errorf("open release archive: %w", err)
	}
	defer z.Close()
	bounded := &io.LimitedReader{R: z, N: 2*maxUpdateSize + 1}
	archive := tar.NewReader(bounded)
	out, err := os.CreateTemp(filepath.Dir(path), ".q-agent-unpack-*")
	if err != nil {
		return "", err
	}
	defer out.Close()
	defer func() {
		if err != nil {
			_ = os.Remove(out.Name())
		}
	}()
	found := false
	for entries := 0; ; entries++ {
		header, nextErr := archive.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return "", fmt.Errorf("read release archive: %w", nextErr)
		}
		if entries >= 1024 || header.Size < 0 || header.Size > maxUpdateSize {
			return "", errors.New("release archive exceeds entry or size limit")
		}
		if header.Name == "q-agent" || header.Name == "./q-agent" {
			if found || header.Typeflag != tar.TypeReg || header.Size == 0 {
				return "", errors.New("release archive must contain one nonempty regular q-agent executable")
			}
			found = true
			if _, err = io.Copy(out, archive); err != nil {
				return "", err
			}
		} else if _, err = io.Copy(io.Discard, archive); err != nil {
			return "", err
		}
	}
	// Drain through EOF to verify gzip's checksum and the total inflation bound.
	if _, err = io.Copy(io.Discard, bounded); err != nil {
		return "", fmt.Errorf("verify release archive: %w", err)
	}
	if !found || bounded.N <= 0 {
		return "", errors.New("release archive missing q-agent or exceeds unpacked size limit")
	}
	if err = out.Chmod(mode); err != nil {
		return "", err
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	return out.Name(), nil
}

func validateELF(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("update is not an ELF executable: %w", err)
	}
	defer f.Close()
	wanted := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if f.Machine != wanted || f.Class != elf.ELFCLASS64 || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return errors.New("update executable has an incompatible architecture or type")
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("service updates require a statically linked executable")
		}
	}
	return nil
}

func validateDownloadURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil {
		return errors.New("release downloads must use https://github.com")
	}
	if !strings.HasPrefix(parsed.Path, "/DemonGiggle/q-agent/releases/download/") {
		return errors.New("URL is not a q-agent GitHub release download")
	}
	return nil
}

func executableTarget(path string) (executable, target string, mode os.FileMode, err error) {
	executable = path
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return "", "", 0, fmt.Errorf("find q-agent executable: %w", err)
		}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", "", 0, fmt.Errorf("resolve q-agent executable: %w", err)
	}
	target = executable
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		target = resolved
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", "", 0, fmt.Errorf("inspect q-agent executable %q: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", 0, fmt.Errorf("q-agent executable %q is not a regular file", target)
	}
	mode = info.Mode().Perm()
	if mode == 0 {
		mode = 0755
	}
	return executable, target, mode, nil
}

func downloadAsset(ctx context.Context, client *http.Client, rawURL, directory string, mode os.FileMode, validator func(string) error) (path string, err error) {
	if validator == nil {
		validator = validateDownloadURL
	}
	if err := validator(rawURL); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("create update download request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "q-agent-update")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("download update: GitHub returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	temporaryFile, err := os.CreateTemp(directory, ".q-agent-update-*")
	if err != nil {
		return "", fmt.Errorf("create update temporary file: %w", err)
	}
	path = temporaryFile.Name()
	temporaryPath := path
	defer func() {
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err = io.Copy(temporaryFile, io.LimitReader(response.Body, maxUpdateSize+1)); err != nil {
		temporaryFile.Close()
		return "", fmt.Errorf("write update temporary file: %w", err)
	}
	if info, statErr := temporaryFile.Stat(); statErr != nil {
		temporaryFile.Close()
		return "", fmt.Errorf("inspect update temporary file: %w", statErr)
	} else if info.Size() == 0 {
		temporaryFile.Close()
		return "", errors.New("downloaded update is empty")
	} else if info.Size() > maxUpdateSize {
		temporaryFile.Close()
		return "", fmt.Errorf("downloaded update exceeds %d bytes", maxUpdateSize)
	}
	if err = temporaryFile.Chmod(mode); err != nil {
		temporaryFile.Close()
		return "", fmt.Errorf("set update permissions: %w", err)
	}
	if err = temporaryFile.Sync(); err != nil {
		temporaryFile.Close()
		return "", fmt.Errorf("sync update temporary file: %w", err)
	}
	if err = temporaryFile.Close(); err != nil {
		return "", fmt.Errorf("close update temporary file: %w", err)
	}
	return path, nil
}

func installUnix(temporary, target string, mode os.FileMode) error {
	if err := os.Chmod(temporary, mode); err != nil {
		return fmt.Errorf("set update permissions: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("replace q-agent executable: %w", err)
	}
	// The rename is already durable enough for normal use; directory sync is a
	// best-effort improvement for filesystems that support it.
	directory, err := os.Open(filepath.Dir(target))
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

type semanticVersion struct {
	major int64
	minor int64
	patch int64
	pre   []string
}

func parseVersion(raw string) (semanticVersion, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 0 && (raw[0] == 'v' || raw[0] == 'V') {
		raw = raw[1:]
	}
	raw = strings.SplitN(raw, "+", 2)[0]
	parts := strings.SplitN(raw, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 2 && len(core) != 3 {
		return semanticVersion{}, false
	}
	values := make([]int64, 3)
	for i, part := range core {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil || value < 0 {
			return semanticVersion{}, false
		}
		values[i] = value
	}
	result := semanticVersion{major: values[0], minor: values[1], patch: values[2]}
	if len(parts) == 2 {
		if parts[1] == "" {
			return semanticVersion{}, false
		}
		for _, identifier := range strings.Split(parts[1], ".") {
			if identifier == "" {
				return semanticVersion{}, false
			}
			result.pre = append(result.pre, identifier)
		}
	}
	return result, true
}

func compareVersions(left, right semanticVersion) int {
	for _, pair := range [][2]int64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.pre) == 0 && len(right.pre) == 0 {
		return 0
	}
	if len(left.pre) == 0 {
		return 1
	}
	if len(right.pre) == 0 {
		return -1
	}
	for i := 0; i < len(left.pre) && i < len(right.pre); i++ {
		leftID, rightID := left.pre[i], right.pre[i]
		leftNumber, leftErr := strconv.ParseInt(leftID, 10, 64)
		rightNumber, rightErr := strconv.ParseInt(rightID, 10, 64)
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		case leftID < rightID:
			return -1
		case leftID > rightID:
			return 1
		}
	}
	if len(left.pre) < len(right.pre) {
		return -1
	}
	if len(left.pre) > len(right.pre) {
		return 1
	}
	return 0
}
