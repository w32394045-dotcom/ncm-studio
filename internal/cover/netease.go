package cover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The official render endpoint accepts a size hint, and the artwork it serves
// is often larger than the copy embedded in the .ncm container — which is the
// point of fetching it rather than just reusing what the file already has.
const (
	songDetailURL = "https://music.163.com/api/song/detail"
	artworkSize   = "?param=1024y1024"
	coverTimeout  = 30 * time.Second
	// Album art is a few hundred kilobytes; this is a sanity bound so a
	// misdirected response cannot be written into the library.
	maxArtworkBytes = 32 << 20
)

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36"

// FetchOfficial downloads the album art NetEase serves for a song.
//
// The url comes from the public song-detail endpoint, which needs no login. The
// caller is expected to have a song id from the .ncm metadata or from a tag
// previously written into the file.
func FetchOfficial(ctx context.Context, client *http.Client, musicID int64) ([]byte, string, error) {
	if musicID <= 0 {
		return nil, "", fmt.Errorf("cover: %d is not a usable song id", musicID)
	}
	if client == nil {
		client = &http.Client{Timeout: coverTimeout}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, coverTimeout)
		defer cancel()
	}

	artURL, err := officialArtworkURL(ctx, client, musicID)
	if err != nil {
		return nil, "", err
	}
	// Ask for a large render first; if the service will not serve one, the
	// original url still resolves.
	data, mime, err := download(ctx, client, artURL+artworkSize)
	if err != nil {
		data, mime, err = download(ctx, client, artURL)
		if err != nil {
			return nil, "", err
		}
	}
	return data, normaliseMIME(mime, data), nil
}

func officialArtworkURL(ctx context.Context, client *http.Client, musicID int64) (string, error) {
	endpoint := fmt.Sprintf("%s?ids=[%d]", songDetailURL, musicID)
	body, _, err := download(ctx, client, endpoint)
	if err != nil {
		return "", fmt.Errorf("cover: look up song %d: %w", musicID, err)
	}

	var payload struct {
		Songs []struct {
			Album struct {
				PicURL string `json:"picUrl"`
			} `json:"album"`
		} `json:"songs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("cover: song %d: unexpected response: %w", musicID, err)
	}
	if len(payload.Songs) == 0 || payload.Songs[0].Album.PicURL == "" {
		return "", fmt.Errorf("cover: song %d has no artwork", musicID)
	}
	return payload.Songs[0].Album.PicURL, nil
}

func download(ctx context.Context, client *http.Client, rawURL string) ([]byte, string, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, "", fmt.Errorf("cover: bad url %q: %w", rawURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Referer", "https://music.163.com/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cover: %s returned %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtworkBytes))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("cover: %s returned an empty body", rawURL)
	}
	// A Content-Type may carry parameters ("image/jpeg; charset=binary"), which
	// would not match any of the types the library stores.
	mime, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return data, strings.TrimSpace(mime), nil
}
