package telegram

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"q-agent/internal/state"
)

var imageMDRe = regexp.MustCompile(`!\[([^\]\n]*)\]\(([^)\s]+?)(?:\s+"[^"]*")?\)`)

// MaxPhotosPerReply caps native photo posts per answer.
const MaxPhotosPerReply = 10

// ExtractPhotos finds Markdown images in the final answer. Valid images become
// native photo attachments; invalid ones become fallback text notes. It returns
// photos in order, cleaned text with image markup removed, and assumes caller
// queues photos first then text (delivery order).
// workspaceRoot is the host path from Workspaces.Ensure (sandbox rootfs or host
// workspace dir).
func ExtractPhotos(answer, workspaceRoot string) (photos []state.PhotoAttachment, cleaned string, notes []string) {
	matches := imageMDRe.FindAllStringSubmatchIndex(answer, -1)
	if len(matches) == 0 {
		return nil, answer, nil
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		if len(photos) >= MaxPhotosPerReply {
			break
		}
		fullStart, fullEnd := m[0], m[1]
		altStart, altEnd := m[2], m[3]
		srcStart, srcEnd := m[4], m[5]
		alt := strings.TrimSpace(answer[altStart:altEnd])
		src := strings.TrimSpace(answer[srcStart:srcEnd])
		out.WriteString(answer[last:fullStart])
		last = fullEnd

		// Remote URL: pass through to Telegram (no local download).
		if strings.HasPrefix(src, "https://") {
			if !validPhotoURL(src) {
				notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: invalid URL. Link: "+src)
				out.WriteString("\n[" + displayAlt(alt, src) + "](" + src + ")\n")
				continue
			}
			caption := Escape(alt)
			// Keep fallback as HTML link for delivery-time failure path.
			fallbackHTML := `<a href="` + escapeAttr(src) + `">` + Escape(displayAlt(alt, src)) + `</a>`
			photos = append(photos, state.PhotoAttachment{
				RemoteURL: src,
				Caption:   caption,
				Fallback:  fallbackHTML + " (photo unavailable)",
			})
			continue
		}
		// Reject exotic schemes (http, data, file, tg, etc.).
		if strings.Contains(src, "://") {
			notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: only https:// URLs and workspace files are supported ("+src+").")
			out.WriteString("\nImage " + displayAlt(alt, src) + " could not be posted: unsupported source.\n")
			continue
		}
		localPath, err := resolvePhotoPath(workspaceRoot, src)
		if err != nil {
			notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: "+err.Error()+".")
			out.WriteString("\nImage " + displayAlt(alt, src) + " (" + src + ") could not be posted: " + err.Error() + ".\n")
			continue
		}
		ext := strings.ToLower(filepath.Ext(localPath))
		if !allowedPhotoExt[ext] {
			notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: unsupported type "+ext+" (use jpg, png, webp, gif).")
			out.WriteString("\nImage " + displayAlt(alt, src) + " could not be posted: unsupported type.\n")
			continue
		}
		st, err := os.Stat(localPath)
		if err != nil {
			notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: file not found ("+src+").")
			out.WriteString("\nImage " + displayAlt(alt, src) + " (" + src + ") could not be posted: file not found.\n")
			continue
		}
		if !st.Mode().IsRegular() {
			notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: not a regular file.")
			out.WriteString("\nImage " + displayAlt(alt, src) + " could not be posted: not a regular file.\n")
			continue
		}
		if st.Size() == 0 || st.Size() > MaxPhotoBytes {
			notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: file size must be 1 byte–10MB.")
			out.WriteString("\nImage " + displayAlt(alt, src) + " could not be posted: file size must be 1 byte-10MB.\n")
			continue
		}
		// Symlink escape: resolve and re-check containment.
		if resolved, err := filepath.EvalSymlinks(localPath); err == nil {
			if !withinDir(workspaceBase(workspaceRoot), resolved) && !withinDir(workspaceRoot, resolved) {
				notes = append(notes, "Image "+quoteAlt(alt)+" could not be posted as a photo: path escapes workspace.")
				out.WriteString("\nImage " + displayAlt(alt, src) + " could not be posted: path escapes workspace.\n")
				continue
			}
			localPath = resolved
		}
		caption := Escape(alt)
		fallbackHTML := Escape("Image "+displayAlt(alt, src)+" ("+src+")") + " (photo unavailable)"
		photos = append(photos, state.PhotoAttachment{
			LocalPath: localPath,
			Caption:   caption,
			Fallback:  fallbackHTML,
		})
		// Valid photo: drop markup entirely; caption travels with the photo.
	}
	out.WriteString(answer[last:])
	cleaned = strings.TrimSpace(out.String())
	// Collapse 3+ newlines left by removals.
	for strings.Contains(cleaned, "\n\n\n") {
		cleaned = strings.ReplaceAll(cleaned, "\n\n\n", "\n\n")
	}
	return photos, cleaned, notes
}

func quoteAlt(alt string) string {
	if alt == "" {
		return "(no caption)"
	}
	return `"` + alt + `"`
}

func displayAlt(alt, src string) string {
	if alt != "" {
		return alt
	}
	if src != "" {
		return src
	}
	return "image"
}

// workspaceBase returns the agent-visible file root for local resolution:
// sandbox rootfs -> <root>/workspace, host workspace -> root itself.
func workspaceBase(workspaceRoot string) string {
	if strings.HasSuffix(filepath.Clean(workspaceRoot), "rootfs") {
		return filepath.Join(workspaceRoot, "workspace")
	}
	return workspaceRoot
}

func withinDir(dir, target string) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if dir == target {
		return true
	}
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolvePhotoPath(workspaceRoot, src string) (string, error) {
	if workspaceRoot == "" {
		return "", errPhoto("workspace unavailable")
	}
	if strings.Contains(src, "\x00") {
		return "", errPhoto("invalid path")
	}
	base := workspaceBase(workspaceRoot)
	var rel string
	switch {
	case src == "/workspace" || src == "/workspace/":
		return "", errPhoto("no file specified")
	case strings.HasPrefix(src, "/workspace/"):
		rel = strings.TrimPrefix(src, "/workspace/")
	case filepath.IsAbs(src):
		cleaned := filepath.Clean(src)
		// Absolute inside workspace root (host mode) is allowed.
		if withinDir(base, cleaned) || withinDir(workspaceRoot, cleaned) {
			return cleaned, nil
		}
		return "", errPhoto("absolute paths outside the workspace are not allowed")
	default:
		rel = src
	}
	if rel == "" {
		return "", errPhoto("no file specified")
	}
	joined := filepath.Join(base, filepath.Clean("/"+rel))
	// filepath.Join with Clean("/"+rel) prevents ../ escape above base, but verify.
	if !withinDir(base, joined) && filepath.Clean(joined) != filepath.Clean(base) {
		return "", errPhoto("path escapes workspace")
	}
	return joined, nil
}

type photoError string

func (e photoError) Error() string { return string(e) }
func errPhoto(s string) error      { return photoError(s) }
