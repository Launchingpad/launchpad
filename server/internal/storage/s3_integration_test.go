package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/storage"
	"devops/internal/storage/storagetest"
)

func put(t *testing.T, up storage.Upload, contentType string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(up.Method, up.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestS3_RoundTrip runs the whole presigned flow against the real silo: the
// signed PUT, the HEAD, the signed GET with response overrides, DELETE. It
// also proves the store enforces the signed headers: another size or another
// content type is refused (the 100 MB limit rests on this).
func TestS3_RoundTrip(t *testing.T) {
	s := storagetest.New(t)
	ctx := context.Background()
	key := "org/" + uuid.NewString() + "/2026/" + uuid.NewString() + "/hello.txt"
	body := []byte("hello, silo")

	up := s.PresignPut(key, "text/plain", int64(len(body)), 15*time.Minute)
	t.Run("wrong size is refused", func(t *testing.T) {
		if code := put(t, up, "text/plain", append(body, '!')); code != http.StatusForbidden {
			t.Fatalf("PUT with another size = %d, want 403", code)
		}
	})
	t.Run("wrong type is refused", func(t *testing.T) {
		if code := put(t, up, "text/html", body); code != http.StatusForbidden {
			t.Fatalf("PUT with another type = %d, want 403", code)
		}
	})
	if _, err := s.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("HEAD after refused PUTs: %v, want ErrNotFound", err)
	}

	if code := put(t, up, "text/plain", body); code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", code)
	}
	info, err := s.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(body)) || info.ContentType != "text/plain" {
		t.Fatalf("HEAD = %+v", info)
	}

	get := s.PresignGet(key, "text/plain; charset=utf-8", `attachment; filename="hello.txt"`, time.Minute)
	resp, err := http.Get(get)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("GET = %d %q", resp.StatusCode, got)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="hello.txt"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}

	// A URL with a changed signature is refused.
	tampered := s.PresignGet(key, "", "", time.Minute)
	tampered = tampered[:len(tampered)-1] + strconv.Itoa((int(tampered[len(tampered)-1])+1)%10)
	if resp, err := http.Get(tampered); err != nil {
		t.Fatal(err)
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("tampered signature = %d, want 403", resp.StatusCode)
		}
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("HEAD after delete: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// TestS3_BadCredentials: a wrong secret is a hard error, not "not found".
func TestS3_BadCredentials(t *testing.T) {
	storagetest.New(t) // gate + bucket
	cfg := storagetest.Config()
	cfg.SecretKey = "wrong-secret-0000"
	s, err := storage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Head(context.Background(), "org/x/2026/y/z.txt"); err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("HEAD with a wrong secret: %v, want a non-NotFound error", err)
	}
	if err := s.CheckBucket(context.Background()); err == nil {
		t.Fatal("CheckBucket with a wrong secret passed")
	}
}

// TestS3_Put: the server-side PUT (the importer's) stores the bytes with
// their type, and the signed length refuses a body of another size.
func TestS3_Put(t *testing.T) {
	s := storagetest.New(t)
	ctx := context.Background()
	key := "org/" + uuid.NewString() + "/2026/" + uuid.NewString() + "/shot.png"
	body := []byte("\x89PNG not really")
	if err := s.Put(ctx, key, "image/png", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	info, err := s.Head(ctx, key)
	if err != nil || info.Size != int64(len(body)) || info.ContentType != "image/png" {
		t.Fatalf("HEAD = %+v, %v", info, err)
	}
	// A body shorter than the signed length never completes.
	short := "org/" + uuid.NewString() + "/2026/" + uuid.NewString() + "/short.png"
	if err := s.Put(ctx, short, "image/png", int64(len(body))+5, bytes.NewReader(body)); err == nil {
		t.Fatal("a PUT shorter than its signed length succeeded")
	}
	if _, err := s.Head(ctx, short); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("HEAD of the refused PUT: %v, want ErrNotFound", err)
	}
}
