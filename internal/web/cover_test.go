package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ncm-studio/internal/cover"
	"ncm-studio/internal/job"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// coverServer is newTestServer plus the handles the cover tests need: the
// working directory and the cover store, so an assertion can look at what was
// actually written rather than only at what the API said.
type coverServer struct {
	*httptest.Server
	dir       string
	workspace string
}

func newCoverServer(t *testing.T) coverServer {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "music")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(root, "cfg"), store.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *store.Config) {
		c.Dir, c.Output, c.Workspace, c.Workers = dir, dir, root, 1
	}); err != nil {
		t.Fatal(err)
	}

	pool := job.New(1, func(ctx context.Context, it *job.Item, r job.Reporter) (job.Result, error) {
		return job.Result{}, nil
	})
	srv := httptest.NewServer(newServerWith(st, pool, nil).Handler())
	t.Cleanup(func() {
		pool.Cancel()
		srv.Close()
	})
	return coverServer{Server: srv, dir: dir, workspace: root}
}

// track writes a playable-shaped FLAC carrying a cover and lyrics, then returns
// its path and the bytes of the artwork inside it.
func (c coverServer) track(t *testing.T, name string, art []byte, musicID int64) (string, []byte) {
	t.Helper()

	audio := make([]byte, 150_000)
	for i := range audio {
		audio[i] = byte(i * 13)
	}
	streamInfo := append([]byte{0, 0, 0, 34}, make([]byte, 34)...)
	prefix := tag.BuildFLACMetadata(&tag.FLACSource{StreamInfo: streamInfo}, &tag.Tags{
		Title:     "Danza Kuduro",
		Artists:   []string{"Lucenzo"},
		Lyrics:    "[00:01.00]La mano arriba",
		Cover:     art,
		CoverMIME: "image/jpeg",
		MusicID:   musicID,
	})

	path := filepath.Join(c.dir, name)
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, audio
}

func (c coverServer) manifest(t *testing.T, path string) coverManifest {
	t.Helper()
	var m coverManifest
	getJSON(t, c.URL+"/api/cover?path="+path, &m)
	return m
}

func (c coverServer) post(t *testing.T, endpoint string, payload any, want int) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(c.URL+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s: status %d, want %d (%s)", endpoint, resp.StatusCode, want, out)
	}
	return resp.StatusCode, out
}

func entryByKind(m coverManifest, kind cover.Kind) (cover.Entry, bool) {
	for _, e := range m.Entries {
		if e.Kind == kind {
			return e, true
		}
	}
	return cover.Entry{}, false
}

// TestManifestFilesTheExistingCoverAsTheOriginal covers the promise the user
// asked for by name: the artwork a track arrived with must remain recoverable,
// so the first time the editor opens a file that artwork is captured.
func TestManifestFilesTheExistingCoverAsTheOriginal(t *testing.T) {
	c := newCoverServer(t)
	art := jpegFixture(101)
	path, _ := c.track(t, "song.flac", art, 0)

	m := c.manifest(t, path)
	if !m.HasCover {
		t.Fatal("the track's own cover was not seen")
	}
	if m.AudioHash == "" {
		t.Error("no audio fingerprint came back")
	}
	orig, ok := entryByKind(m, cover.KindOriginal)
	if !ok {
		t.Fatalf("no original entry: %+v", m.Entries)
	}
	if orig.ID != cover.IDOriginal || orig.Size != int64(len(art)) {
		t.Errorf("original = %+v, want the %d-byte image under id %q", orig, len(art), cover.IDOriginal)
	}
	if m.Current != cover.IDOriginal {
		t.Errorf("current = %q, want %q", m.Current, cover.IDOriginal)
	}

	// Looking again must not re-file it or grow the history.
	again := c.manifest(t, path)
	if len(again.Entries) != 1 {
		t.Errorf("%d entries on the second look, want 1", len(again.Entries))
	}
}

// TestManifestForACoverlessTrackListsNoEntries pins the wire shape of an empty
// library. The page filters this list, and null is not "no covers" to a filter
// — it is a crash, on exactly the tracks that have no artwork to show.
func TestManifestForACoverlessTrackListsNoEntries(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "bare.flac", nil, 0)

	resp, err := http.Get(c.URL + "/api/cover?path=" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var m coverManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.HasCover {
		t.Fatal("the fixture track was supposed to carry no artwork")
	}
	if !bytes.Contains(body, []byte(`"entries":[]`)) {
		t.Errorf("an empty library must still be a list, got %s", body)
	}
}

func TestApplyUploadReplacesCoverAndKeepsMine(t *testing.T) {
	c := newCoverServer(t)
	original := jpegFixture(11)
	path, audio := c.track(t, "song.flac", original, 0)
	before := c.manifest(t, path)

	mine := jpegFixture(222)
	status, body := c.postUpload(t, path, "mine.jpg", mine, http.StatusOK)

	var m coverManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("apply returned %s: %v", body, err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if m.Current == cover.IDOriginal {
		t.Error("the file is still reported as carrying the original")
	}
	if len(m.Entries) != len(before.Entries)+1 {
		t.Errorf("history has %d entries, want %d", len(m.Entries), len(before.Entries)+1)
	}

	// The file itself must now carry my image, and the audio must be untouched.
	got, err := tag.ReadCover(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, mine) {
		t.Error("the file does not carry the image that was applied")
	}

	after := readFile(t, path)
	if !bytes.HasSuffix(after, audio) {
		t.Error("the audio payload changed when the cover was replaced")
	}
	if id := tag.ReadMusicID(path); id != 0 {
		t.Errorf("music id = %d, want it left unset", id)
	}
}

// TestApplyByIdSwitchesBack is the "quickly switch back" half of the request:
// every image the library holds stays applicable, including the original.
func TestApplyByIdSwitchesBack(t *testing.T) {
	c := newCoverServer(t)
	original := jpegFixture(31)
	path, _ := c.track(t, "song.flac", original, 0)
	c.manifest(t, path)

	c.postUpload(t, path, "mine.jpg", jpegFixture(32), http.StatusOK)

	body := c.postJSONBody(t, "/api/cover/apply",
		map[string]string{"path": path, "id": cover.IDOriginal}, http.StatusOK)
	var m coverManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Current != cover.IDOriginal {
		t.Errorf("current = %q, want the original", m.Current)
	}
	got, err := tag.ReadCover(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, original) {
		t.Error("switching back did not restore the original artwork")
	}

	// And back to mine, which must reuse the entry rather than adding another.
	mine, _ := entryByKind(m, cover.KindHistory)
	body = c.postJSONBody(t, "/api/cover/apply",
		map[string]string{"path": path, "id": mine.ID}, http.StatusOK)
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Errorf("%d entries after switching back and forth, want 2: %+v", len(m.Entries), m.Entries)
	}
	if m.Current != mine.ID {
		t.Errorf("current = %q, want %q", m.Current, mine.ID)
	}
}

func TestForgetRefusesTheOriginalAndTheImageInUse(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "song.flac", jpegFixture(41), 0)
	c.manifest(t, path)
	_, applied := c.postUpload(t, path, "mine.jpg", jpegFixture(42), http.StatusOK)

	var m coverManifest
	if err := json.Unmarshal(applied, &m); err != nil {
		t.Fatal(err)
	}
	mine, ok := entryByKind(m, cover.KindHistory)
	if !ok {
		t.Fatal("the applied image is not in the library")
	}

	// The original is the one image that cannot be recovered from anywhere
	// else, so deleting it is refused outright.
	body := c.postJSONBody(t, "/api/cover/forget",
		map[string]string{"path": path, "id": cover.IDOriginal}, http.StatusConflict)
	if !strings.Contains(string(body), "original") {
		t.Errorf("refusal reads %q, want it to name the original", body)
	}

	// The image the file is using cannot go either, or the list would disagree
	// with the file.
	c.postJSONBody(t, "/api/cover/forget",
		map[string]string{"path": path, "id": mine.ID}, http.StatusConflict)

	// Switch away first, and then it can go.
	c.postJSONBody(t, "/api/cover/apply",
		map[string]string{"path": path, "id": cover.IDOriginal}, http.StatusOK)
	body = c.postJSONBody(t, "/api/cover/forget",
		map[string]string{"path": path, "id": mine.ID}, http.StatusOK)

	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, still := entryByKind(m, cover.KindHistory); still {
		t.Error("the image is still in the library after being deleted")
	}
	if _, kept := entryByKind(m, cover.KindOriginal); !kept {
		t.Error("deleting one image took the original with it")
	}
}

func TestRemoveStripsArtworkButKeepsTheLibrary(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "song.flac", jpegFixture(51), 0)
	before := c.manifest(t, path)

	body := c.postJSONBody(t, "/api/cover/remove", map[string]string{"path": path}, http.StatusOK)
	var m coverManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.HasCover {
		t.Error("the file still has a cover")
	}
	if m.Current != "" {
		t.Errorf("current = %q, want empty", m.Current)
	}
	if len(m.Entries) != len(before.Entries) {
		t.Error("removing the artwork from the file also emptied the library")
	}

	// The stripped artwork must still be applicable afterwards.
	c.postJSONBody(t, "/api/cover/apply",
		map[string]string{"path": path, "id": cover.IDOriginal}, http.StatusOK)
	if got, err := tag.ReadCover(path); err != nil || got == nil {
		t.Errorf("the original could not be put back: %v", err)
	}
}

func TestCoverImageServesEntriesAndCurrent(t *testing.T) {
	c := newCoverServer(t)
	original := jpegFixture(61)
	path, _ := c.track(t, "song.flac", original, 0)
	c.manifest(t, path)

	for _, id := range []string{"current", cover.IDOriginal} {
		resp, err := http.Get(c.URL + "/api/cover/image?path=" + path + "&id=" + id)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("id %s: status %d (%s)", id, resp.StatusCode, body)
		}
		if !bytes.Equal(body, original) {
			t.Errorf("id %s served %d bytes, want the stored %d", id, len(body), len(original))
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("id %s: content type %q", id, ct)
		}
	}

	resp, err := http.Get(c.URL + "/api/cover/image?path=" + path + "&id=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", resp.StatusCode)
	}
}

func TestFetchIsRefusedWhenNoSongIdIsKnown(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "song.flac", jpegFixture(71), 0)

	body := c.postJSONBody(t, "/api/cover/fetch", map[string]string{"path": path}, http.StatusBadRequest)
	if !strings.Contains(string(body), "song id") {
		t.Errorf("refusal reads %q, want it to explain that no song id is known", body)
	}
}

// TestCoverFollowsTheTrackAcrossARename is the reason the library is keyed by
// an audio fingerprint rather than by path.
func TestCoverFollowsTheTrackAcrossARename(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "before.flac", jpegFixture(81), 0)
	c.manifest(t, path)
	c.postUpload(t, path, "mine.jpg", jpegFixture(82), http.StatusOK)
	before := c.manifest(t, path)

	moved := filepath.Join(c.dir, "after.flac")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}

	after := c.manifest(t, moved)
	if after.AudioHash != before.AudioHash {
		t.Error("the fingerprint moved with the name")
	}
	if len(after.Entries) != len(before.Entries) {
		t.Errorf("%d entries after the rename, want %d: the history was orphaned",
			len(after.Entries), len(before.Entries))
	}
	if after.Current != before.Current {
		t.Errorf("current = %q after the rename, want %q", after.Current, before.Current)
	}
}

// TestWorkspaceChangeMovesTheLibrary is what makes the setup screen honest: a
// workspace picked at first run has to be where the covers actually land, not
// merely what the settings file says until the next restart.
func TestWorkspaceChangeMovesTheLibrary(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "song.flac", jpegFixture(95), 0)
	c.manifest(t, path)
	c.postUpload(t, path, "mine.jpg", jpegFixture(96), http.StatusOK)

	first := filepath.Join(c.workspace, "covers")
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("the library was not created in the workspace: %v", err)
	}

	moved := filepath.Join(c.workspace, "elsewhere")
	c.postJSONBody(t, "/api/config", map[string]any{"workspace": moved}, http.StatusOK)

	// A fresh track gives the new workspace something to hold, since the
	// library is built on demand rather than moved wholesale.
	other, _ := c.track(t, "second.flac", jpegFixture(97), 0)
	c.manifest(t, other)

	if _, err := os.Stat(filepath.Join(moved, "covers")); err != nil {
		t.Errorf("the library did not follow the workspace: %v", err)
	}
}

func TestCoverRejectsNonImageUpload(t *testing.T) {
	c := newCoverServer(t)
	path, _ := c.track(t, "song.flac", jpegFixture(91), 0)
	c.manifest(t, path)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("path", path); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "not-an-image.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("this is not a picture")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(c.URL+"/api/cover/apply", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func (c coverServer) postJSONBody(t *testing.T, endpoint string, payload any, want int) []byte {
	t.Helper()
	_, body := c.post(t, endpoint, payload, want)
	return body
}

func (c coverServer) postUpload(t *testing.T, path, name string, art []byte, want int) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("path", path); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(art); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(c.URL+"/api/cover/apply", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("upload %s: status %d, want %d (%s)", name, resp.StatusCode, want, body)
	}
	return resp.StatusCode, body
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// jpegFixture is a small but genuinely decodable JPEG, so MIME sniffing and the
// image handler are exercised on something real.
func jpegFixture(seed byte) []byte {
	img := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
	}
	img = append(img, []byte{0xFF, 0xDB, 0x00, 0x43, 0x00}...)
	for i := 0; i < 64; i++ {
		img = append(img, byte(i))
	}
	img = append(img, []byte{0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00, 0x08, 0x00, 0x08, 0x01, 0x01, 0x11, 0x00}...)
	img = append(img, []byte{0xFF, 0xC4, 0x00, 0x1F, 0x00}...)
	for i := 0; i < 28; i++ {
		img = append(img, seed+byte(i))
	}
	img = append(img, []byte{0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3F, 0x00}...)
	// A run of entropy-coded bytes long enough to make each fixture distinct.
	img = append(img, bytes.Repeat([]byte{seed, seed ^ 0x5A, 0x7F, 0x00}, 512)...)
	return append(img, 0xFF, 0xD9)
}
