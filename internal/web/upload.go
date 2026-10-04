package web

import (
	"io"
	"mime/multipart"
	"net/url"
	"os"
)

// saveUploadedFile streams a multipart file to dst. The body is copied rather
// than buffered, because a single .ncm can run to hundreds of megabytes.
func saveUploadedFile(h *multipart.FileHeader, dst string) error {
	src, err := h.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// urlEscape percent-encodes a file name for a Content-Disposition header.
func urlEscape(s string) string {
	return url.PathEscape(s)
}
