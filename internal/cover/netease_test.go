package cover

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubTransport answers the NetEase calls without a network, and records what
// was asked for so the size hint can be asserted.
type stubTransport struct {
	detail  string // body for the song-detail endpoint
	detailN int    // status for it, 200 when zero
	art     []byte
	artType string
	// artN rejects the sized render when non-zero, which is how the fallback to
	// the plain url gets exercised.
	artN int

	asked []string
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.asked = append(s.asked, req.URL.String())

	body, status, ctype := "", http.StatusOK, "application/json"
	switch {
	case strings.Contains(req.URL.Path, "/api/song/detail"):
		body, status = s.detail, s.detailN
	default:
		if s.artN != 0 && req.URL.RawQuery != "" {
			status = s.artN
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {s.artType}},
			Body:       io.NopCloser(bytes.NewReader(s.art)),
			Request:    req,
		}, nil
	}
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {ctype}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func clientFor(s *stubTransport) *http.Client { return &http.Client{Transport: s} }

func TestFetchOfficialPrefersTheLargeRender(t *testing.T) {
	art := img(7, 4000)
	st := &stubTransport{
		detail:  `{"songs":[{"album":{"picUrl":"http://cdn.example/art.jpg"}}]}`,
		art:     art,
		artType: "image/jpeg; charset=binary",
	}

	data, mime, err := FetchOfficial(context.Background(), clientFor(st), 42)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, art) {
		t.Error("the artwork body did not come back intact")
	}
	// The content type carries parameters on the wire; the library only stores
	// bare types, so they have to be stripped before it is handed on.
	if mime != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", mime)
	}
	if len(st.asked) < 2 {
		t.Fatalf("only %d requests: %v", len(st.asked), st.asked)
	}
	if !strings.Contains(st.asked[1], "1024y1024") {
		t.Errorf("artwork requested as %q, want the sized render", st.asked[1])
	}
}

func TestFetchOfficialFallsBackWhenSizedRenderFails(t *testing.T) {
	art := img(3, 900)
	st := &stubTransport{
		detail:  `{"songs":[{"album":{"picUrl":"http://cdn.example/art.jpg"}}]}`,
		art:     art,
		artType: "image/png",
		artN:    http.StatusForbidden,
	}

	data, mime, err := FetchOfficial(context.Background(), clientFor(st), 7)
	if err != nil {
		t.Fatalf("the fallback did not happen: %v", err)
	}
	if !bytes.Equal(data, art) || mime != "image/png" {
		t.Errorf("got %d bytes of %q, want the %d-byte png", len(data), mime, len(art))
	}
}

func TestFetchOfficialReportsMissingArtwork(t *testing.T) {
	for name, body := range map[string]string{
		"no songs":     `{"songs":[]}`,
		"no pic url":   `{"songs":[{"album":{}}]}`,
		"not json":     `<html>login</html>`,
		"empty object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := &stubTransport{detail: body}
			if _, _, err := FetchOfficial(context.Background(), clientFor(st), 5); err == nil {
				t.Error("a response with no artwork was accepted")
			}
		})
	}
}

func TestFetchOfficialRejectsUnusableID(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, _, err := FetchOfficial(context.Background(), clientFor(&stubTransport{}), id); err == nil {
			t.Errorf("song id %d was accepted", id)
		}
	}
}

// TestNormaliseMIMECanonicalisesAliases covers the spelling NetEase actually
// answers with. The value ends up in a FLAC PICTURE block, where "image/jpg" is
// not a MIME type the format defines and a strict player may drop the artwork.
func TestNormaliseMIMECanonicalisesAliases(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"image/jpg", "image/jpeg"},
		{"IMAGE/JPG", "image/jpeg"},
		{"image/jpg; charset=binary", "image/jpeg"},
		{"  image/png  ", "image/png"},
		{"image/x-png", "image/png"},
		{"", "image/jpeg"},
		{"text/plain", "image/jpeg"}, // sniffed from the bytes instead
		{"application/octet-stream", "image/jpeg"},
	} {
		if got := normaliseMIME(tc.in, jpeg); got != tc.want {
			t.Errorf("normaliseMIME(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
