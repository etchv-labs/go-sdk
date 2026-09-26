package etchv

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

type fixture struct{ Extension, Media, Mime, Base64 string }

func testServer(t *testing.T) (string, []fixture) {
	t.Helper()
	raw, err := os.ReadFile("tests/formats.json")
	if err != nil {
		t.Fatal(err)
	}
	var formats []fixture
	if err = json.Unmarshal(raw, &formats); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	keys := map[string]string{}
	job := "req_" + strings.Repeat("b", 64)
	id := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(message string) { t.Error(message); http.Error(w, message, 500) }
		if r.Header.Get("X-API-Key") != "test-key" {
			fail("missing API key")
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			fail("unexpected URL")
			return
		}
		scenario, ext := parts[0], parts[1]
		route := strings.Join(parts[2:], "/")
		var f fixture
		for _, entry := range formats {
			if entry.Extension == ext {
				f = entry
			}
		}
		expected, _ := base64.StdEncoding.DecodeString(f.Base64)
		detection := strings.HasSuffix(route, "/detect") || strings.Contains(route, "detection-jobs/")
		countKey := scenario + "/" + ext + "/" + r.Method
		counts[countKey]++
		if r.Method == "POST" {
			if err := r.ParseMultipartForm(MaxFileSize); err != nil {
				fail(err.Error())
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, h, err := r.FormFile("file")
			if err != nil {
				fail(err.Error())
				return
			}
			defer file.Close()
			b, _ := io.ReadAll(file)
			if !bytes.Equal(b, expected) || h.Filename != "input."+ext {
				fail("upload bytes or filename changed")
				return
			}
			suffix := ""
			if detection {
				suffix = "/detect"
			}
			if route != "watermarks/"+f.Media+suffix {
				fail("wrong endpoint")
				return
			}
			if !detection {
				var data map[string]string
				if json.Unmarshal([]byte(r.FormValue("data")), &data) != nil || data["asset"] != "example" {
					fail("metadata changed")
					return
				}
			}
			if !detection || f.Media == "videos" {
				key := r.Header.Get("Idempotency-Key")
				k := scenario + ext + route
				if key == "" || (keys[k] != "" && keys[k] != key) {
					fail("unstable idempotency key")
					return
				}
				keys[k] = key
			}
		} else {
			prefix := "jobs"
			if detection {
				prefix = "detection-jobs"
			}
			if route != "watermarks/"+prefix+"/"+job+"/result" {
				fail("untrusted poll URL")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", job)
		reply := func(status int, payload any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(payload) }
		switch scenario {
		case "retry":
			if counts[countKey] == 1 {
				reply(503, map[string]string{"detail": "temporary"})
				return
			}
		case "failed":
			reply(503, map[string]string{"status": "failed"})
			return
		case "redirect":
			w.Header().Set("Location", "/forbidden")
			reply(307, map[string]string{})
			return
		case "invalid":
			reply(422, map[string]string{"detail": "unsupported profile"})
			return
		case "bad-job":
			reply(202, map[string]string{"request_id": "../../forbidden"})
			return
		}
		if (scenario == "embed-job" || scenario == "detect-job" || scenario == "deadline") && (r.Method == "POST" || scenario == "deadline") {
			w.Header().Set("Retry-After", "0.01")
			w.Header().Set("Location", "https://untrusted.example/steal")
			reply(202, map[string]string{"request_id": job, "result_url": "https://untrusted.example/steal"})
			return
		}
		if detection {
			units := []map[string]any{{"index": 0, "watermarked": true, "confidence": .99, "watermark_id": id}, {"index": 1, "watermarked": true, "confidence": .99, "watermark_id": id}}
			result := map[string]any{"watermarked": true, "confidence": .99, "watermark_id": id, "units": units}
			if scenario == "bad-detection" {
				result["confidence"] = 1.5
			}
			if scenario == "bad-units" {
				units[1]["index"] = 4
			}
			reply(200, result)
			return
		}
		if scenario == "bad-id" {
			id = "invalid"
		} else {
			id = strings.Repeat("a", 64)
		}
		w.Header().Set("Content-Type", f.Mime)
		w.Header().Set("X-Watermark-ID", id)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"protected.%s\"", ext))
		_, _ = w.Write(expected)
	}))
	t.Cleanup(func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, f := range formats {
			if counts["formats/"+f.Extension+"/POST"] != 2 {
				t.Error("missing format coverage", f.Extension)
			}
		}
		if counts["retry/png/POST"] != 2 || counts["failed/png/POST"] != 1 || counts["embed-job/pdf/GET"] < 1 || counts["detect-job/mp4/GET"] < 1 {
			t.Error("retry/poll coverage missing")
		}
	})
	return server.URL, formats
}
