package keybroker

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Secret is a derived secret, as a workload asks for it.
type Secret struct {
	Name string `json:"name"`
	// Version defaults to 1.
	Version int `json:"version,omitempty"`
	// Bytes defaults to 32.
	Bytes int `json:"bytes,omitempty"`
	// Encoding is "hex", the default, or "base64".
	Encoding string `json:"encoding,omitempty"`
}

// Part is one piece of a File's value: Text, or a derived secret, encoded.
type Part struct {
	Text   string  `json:"text,omitempty"`
	Derive *Secret `json:"derive,omitempty"`
}

// File is an env file in Fetch's directory, called Name, holding
// Variable=<its parts, concatenated>.
type File struct {
	Name     string `json:"name"`
	Variable string `json:"variable"`
	Parts    []Part `json:"parts"`
}

// Files decodes a JSON list of files from an environment variable, through
// envconfig.
type Files []File

// EnvDecode implements envconfig.Decoder.
func (f *Files) EnvDecode(val string) error {
	return json.Unmarshal([]byte(val), f)
}

var variableRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks the files' names and variables, and fills in their
// secrets' defaults.
func (f Files) Validate() error {
	names := map[string]bool{}
	for i := range f {
		file := &f[i]
		if file.Name == "" || strings.ContainsAny(file.Name, "/\x00") || file.Name == "." || file.Name == ".." || !strings.HasSuffix(file.Name, ".env") {
			return fmt.Errorf("file %q: must be a name ending in .env", file.Name)
		}
		if names[file.Name] {
			return fmt.Errorf("file %q: listed twice", file.Name)
		}
		names[file.Name] = true
		if !variableRE.MatchString(file.Variable) {
			return fmt.Errorf("file %q: invalid variable %q", file.Name, file.Variable)
		}
		for j := range file.Parts {
			part := &file.Parts[j]
			if (part.Derive == nil) == (part.Text == "") {
				return fmt.Errorf("file %q: part %d needs text or derive, not both", file.Name, j)
			}
			if strings.ContainsAny(part.Text, "\n\r\x00") {
				return fmt.Errorf("file %q: part %d: text spans lines", file.Name, j)
			}
			if s := part.Derive; s != nil {
				if s.Version == 0 {
					s.Version = 1
				}
				if s.Bytes == 0 {
					s.Bytes = 32
				}
				if s.Encoding == "" {
					s.Encoding = "hex"
				}
				if s.Encoding != "hex" && s.Encoding != "base64" {
					return fmt.Errorf("file %q: part %d: encoding %q: must be hex or base64", file.Name, j, s.Encoding)
				}
			}
		}
	}
	return nil
}

// StatusError is the broker refusing a request.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("broker: %d %s", e.Code, e.Message)
}

// Permanent reports whether retrying the request can't succeed.
func (e *StatusError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500
}

// Client fetches secrets from the broker at URL, through HTTP, which
// authenticates with the workload's SVID.
type Client struct {
	URL  string
	HTTP *http.Client
}

// Fetch returns s, raw.
func (c *Client) Fetch(ctx context.Context, s Secret) ([]byte, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	u = u.JoinPath("v1/secrets", s.Name)
	u.RawQuery = url.Values{
		"version": {strconv.Itoa(s.Version)},
		"bytes":   {strconv.Itoa(s.Bytes)},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	if len(body) != s.Bytes {
		return nil, fmt.Errorf("%s: %d bytes, want %d", s.Name, len(body), s.Bytes)
	}
	return body, nil
}

// Render returns each file's contents, fetching each secret once.
func (c *Client) Render(ctx context.Context, files Files) (map[string][]byte, error) {
	secrets := map[Secret]string{}
	out := map[string][]byte{}
	for _, file := range files {
		var b strings.Builder
		b.WriteString(file.Variable + "=")
		for _, part := range file.Parts {
			if part.Derive == nil {
				b.WriteString(part.Text)
				continue
			}
			s := *part.Derive
			value, ok := secrets[s]
			if !ok {
				key, err := c.Fetch(ctx, s)
				if err != nil {
					return nil, err
				}
				value = encode(key, s.Encoding)
				secrets[s] = value
			}
			b.WriteString(value)
		}
		b.WriteString("\n")
		out[file.Name] = []byte(b.String())
	}
	return out, nil
}

func encode(key []byte, encoding string) string {
	if encoding == "base64" {
		return base64.StdEncoding.EncodeToString(key)
	}
	return hex.EncodeToString(key)
}

// WriteFiles writes each file to dir, readable by its owner only, replacing
// it atomically, and removes the other .env files there, which hold secrets
// the workload no longer lists.
func WriteFiles(dir string, contents map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, data := range contents {
		if err := writeFile(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := contents[e.Name()]; !ok && strings.HasSuffix(e.Name(), ".env") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
