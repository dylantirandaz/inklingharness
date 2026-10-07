package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func fetchInput(address string, offset, maxChars int) json.RawMessage {
	arguments := map[string]any{"url": address}
	if offset != 0 {
		arguments["offset"] = offset
	}
	if maxChars != 0 {
		arguments["max_chars"] = maxChars
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return encoded
}

func runFetch(t *testing.T, input json.RawMessage) tools.Result {
	t.Helper()
	result, err := FetchTool().Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run returned a fault: %v", err)
	}
	if !utf8.ValidString(result.Content) {
		t.Fatalf("result is not valid UTF-8: %q", result.Content)
	}
	return result
}

// serve starts a server that sends body with the given Content-Type. An
// empty content type sends no Content-Type header.
func serve(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if contentType == "" {
			writer.Header()["Content-Type"] = nil
		} else {
			writer.Header().Set("Content-Type", contentType)
		}
		fmt.Fprint(writer, body)
	}))
	t.Cleanup(server.Close)
	return server
}

const testPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title> Test &amp;
 Page </title><style>body { color: red }</style>
<script>var x = "<p>script text</p>";</script></head>
<body>
<h1>Main   Title</h1>
<p>Hello&nbsp;<b>bold</b>   world &lt;3 &eacute;&#233;</p>
<p>See <a href="intro.html">the docs</a>, <a href="/root?a=1&amp;b=2">root</a>,
<a href="https://example.com/x">example</a>, <a href="#top">top</a>, <a href="javascript:alert(1)">js</a>
and <A HREF='mailto:a@example.com'>mail</A>.</p>
<ul><li>one</li><li><p>two</p><ul><li>nested</li></ul></li></ul>
<pre><code>func main() {
	fmt.Println("&lt;hi&gt;")
}
</code></pre>
<p>Use <code>go test</code> now.</p>
<noscript>noscript text</noscript><svg><svg><title>svg title</title></svg><text>svg text</text></svg>
<script type="module">alert("script text 2")</SCRIPT >
<!-- comment text -->
<h2>End</h2><div>a</div><div>b<br>c</div>
</body></html>`

func TestHTMLConversion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/docs/page.html", http.StatusFound)
	})
	mux.HandleFunc("/docs/page.html", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(writer, testPage)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := runFetch(t, fetchInput(server.URL+"/start", 0, 0))
	if result.IsError {
		t.Fatalf("error result: %s", result.Content)
	}
	final := server.URL + "/docs/page.html"
	wantStart := "Title: Test & Page\nURL: " + final + "\n"
	if !strings.HasPrefix(result.Content, wantStart) {
		t.Fatalf("result does not start with %q:\n%s", wantStart, result.Content)
	}
	for _, want := range []string{
		"\n# Main Title\n\nHello bold world <3 éé\n\n",
		"See the docs (" + server.URL + "/docs/intro.html), root (" + server.URL + "/root?a=1&b=2), example (https://example.com/x), top, js and mail (mailto:a@example.com).",
		"\n- one\n- two\n  - nested\n",
		"\n```\nfunc main() {\n\tfmt.Println(\"<hi>\")\n}\n```\n",
		"Use `go test` now.",
		"\n## End\n\na\nb\nc",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("result lacks %q:\n%s", want, result.Content)
		}
	}
	for _, unwanted := range []string{"script text", "color", "noscript text", "svg", "comment text", "<b>", "&amp;", "&nbsp;"} {
		if strings.Contains(result.Content, unwanted) {
			t.Errorf("result contains %q:\n%s", unwanted, result.Content)
		}
	}
}

func TestHTMLEdgeCases(t *testing.T) {
	cases := []struct {
		name, page string
		want       []string
		unwanted   []string
	}{
		{"no title", `<p>text</p>`, []string{"Title: (none)\n", "\n\ntext"}, nil},
		{"head without end tag", `<html><head><title>T</title><meta name=x><body><p>body text`, []string{"Title: T\n", "body text"}, nil},
		{"text closes head", `<head><title>T</title>visible`, []string{"visible"}, nil},
		{"cut inside tag", `<p>before</p><a href="x`, []string{"before"}, []string{"href"}},
		{"cut inside script", `<p>before</p><script>secret`, []string{"before"}, []string{"secret"}},
		{"stray less-than", `<p>a < b and 1<2</p>`, []string{"a < b and 1<2"}, nil},
		{"quoted greater-than", `<a title="x>y" href="/z">link</a>`, []string{"link (http://"}, []string{"y\""}},
		{"backticks in pre", "<pre>a ``` b</pre>", []string{"\n````\na ``` b\n````"}, nil},
		{"pre keeps space", "<pre>  a\n\n    b</pre>", []string{"```\n  a\n\n    b\n```"}, nil},
		{"link without text", `<a href="/img"><img src="i.png"></a>`, []string{"/img"}, []string{"()"}},
		{"link text is the URL", `<a href="https://go.dev/">https://go.dev/</a>`, []string{"\nhttps://go.dev/"}, []string{"(https://go.dev/)"}},
		{"control entity", `<p>a&#27;[31mb&#7;</p>`, []string{"a[31mb"}, []string{"\x1b", "\x07"}},
		{"table cells", `<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>`, []string{"a b\nc"}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := serve(t, "text/html", test.page)
			result := runFetch(t, fetchInput(server.URL, 0, 0))
			if result.IsError {
				t.Fatalf("error result: %s", result.Content)
			}
			for _, want := range test.want {
				if !strings.Contains(result.Content, want) {
					t.Errorf("result lacks %q:\n%s", want, result.Content)
				}
			}
			for _, unwanted := range test.unwanted {
				if strings.Contains(result.Content, unwanted) {
					t.Errorf("result contains %q:\n%s", unwanted, result.Content)
				}
			}
		})
	}
}

func TestTextTypes(t *testing.T) {
	cases := []struct {
		name, contentType, body, want string
	}{
		{"plain", "text/plain", "line 1\nline 2", "\n\nline 1\nline 2"},
		{"markdown", "text/markdown; charset=utf-8", "# Head\n<b>kept</b>", "\n\n# Head\n<b>kept</b>"},
		{"json", "application/json", `{"a":[1,2],"b":"<p>"}`, "\n\n{\"a\":[1,2],\"b\":\"<p>\"}"},
		{"xml", "application/xml", "<a><b>x</b></a>", "\n\n<a><b>x</b></a>"},
		{"structured suffix", "application/ld+json", `{"@id":1}`, `{"@id":1}`},
		{"latin-1", "text/plain; charset=ISO-8859-1", "caf\xe9 \xa9", "café ©"},
		{"windows-1252", "text/plain; charset=windows-1252", "\x93quoted\x94 \x80", "“quoted” €"},
		{"undefined windows-1252 byte", "text/plain; charset=windows-1252", "a\x81b", "ab"},
		{"invalid UTF-8", "text/plain; charset=utf-8", "a\xffb", "a\uFFFDb"},
		{"byte order mark", "text/plain", "\uFEFFstart", "\n\nstart"},
		{"control characters", "text/plain", "a\x1b[31mred\x07\x00\r\nnext\rlast\u009b\x7fend\ttab", "a[31mred\nnext\nlastend\ttab"},
		{"sniffed text", "", "no type here", "no type here"},
		{"sniffed HTML", "", "<!DOCTYPE html><title>Sniffed</title><p>x", "Title: Sniffed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := serve(t, test.contentType, test.body)
			result := runFetch(t, fetchInput(server.URL, 0, 0))
			if result.IsError {
				t.Fatalf("error result: %s", result.Content)
			}
			if !strings.HasPrefix(result.Content, "URL: "+server.URL) && !strings.HasPrefix(result.Content, "Title: ") {
				t.Fatalf("result does not start with the URL:\n%s", result.Content)
			}
			if !strings.Contains(result.Content, test.want) {
				t.Fatalf("result lacks %q:\n%q", test.want, result.Content)
			}
		})
	}
}

func TestRefusedResponses(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		want                    []string
	}{
		{"image", "image/png", "\x89PNG\r\n\x1a\n", []string{"image/png", "not supported"}},
		{"svg image", "image/svg+xml", "<svg/>", []string{"image/svg+xml"}},
		{"pdf", "application/pdf", "%PDF-1.7", []string{"application/pdf"}},
		{"sniffed binary", "", "\x00\x01\x02\x03binary", []string{"application/octet-stream"}},
		{"unsupported charset", "text/plain; charset=Shift_JIS", "x", []string{`"Shift_JIS"`, "not supported"}},
		{"invalid content type", "text/", "x", []string{"not valid"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := serve(t, test.contentType, test.body)
			result := runFetch(t, fetchInput(server.URL, 0, 0))
			if !result.IsError {
				t.Fatalf("result is not an error:\n%s", result.Content)
			}
			for _, want := range test.want {
				if !strings.Contains(result.Content, want) {
					t.Errorf("error lacks %q: %s", want, result.Content)
				}
			}
		})
	}
}

func TestStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprint(writer, "missing\x1b page "+strings.Repeat("x", 10_000)+"END")
	}))
	defer server.Close()
	result := runFetch(t, fetchInput(server.URL+"/gone", 0, 0))
	if !result.IsError {
		t.Fatalf("result is not an error:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "404 Not Found") || !strings.Contains(result.Content, server.URL+"/gone") ||
		!strings.Contains(result.Content, "missing page") {
		t.Fatalf("error lacks status, URL or text: %s", result.Content)
	}
	if strings.Contains(result.Content, "END") || len(result.Content) > maxErrorBytes+200 {
		t.Fatalf("error has more than %d bytes of body: %d bytes", maxErrorBytes, len(result.Content))
	}
}

func TestRedirects(t *testing.T) {
	mux := http.NewServeMux()
	// /hop/N redirects to /hop/N-1; /hop/0 answers.
	mux.HandleFunc("/hop/{count}", func(writer http.ResponseWriter, request *http.Request) {
		var count int
		if _, err := fmt.Sscan(request.PathValue("count"), &count); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if count > 0 {
			http.Redirect(writer, request, fmt.Sprintf("/hop/%d", count-1), http.StatusFound)
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(writer, "arrived; agent ", request.Header.Get("User-Agent"))
	})
	mux.HandleFunc("/to", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", request.URL.Query().Get("target"))
		writer.WriteHeader(http.StatusMovedPermanently)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cases := []struct {
		path    string
		isError bool
		want    string
	}{
		{"/hop/0", false, "arrived; agent " + userAgent},
		{"/hop/5", false, "URL: " + server.URL + "/hop/0\n"},
		{"/hop/6", true, "stopped after 5 redirects"},
		{"/to?target=ftp://example.com/file", true, `redirect to "ftp://example.com/file" refused`},
		{"/to?target=file:///etc/passwd", true, `redirect to "file:///etc/passwd" refused`},
	}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			result := runFetch(t, fetchInput(server.URL+test.path, 0, 0))
			if result.IsError != test.isError || !strings.Contains(result.Content, test.want) {
				t.Fatalf("result error %v, want %v with %q:\n%s", result.IsError, test.isError, test.want, result.Content)
			}
		})
	}
}

func TestRequestHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(writer, "%s %s|%s", request.Method, request.Header.Get("User-Agent"), request.Header.Get("Accept"))
	}))
	defer server.Close()
	result := runFetch(t, fetchInput(server.URL, 0, 0))
	want := "GET " + userAgent + "|" + acceptTypes
	if result.IsError || !strings.HasSuffix(result.Content, want) {
		t.Fatalf("result = %q, want suffix %q", result.Content, want)
	}
	if !strings.HasPrefix(acceptTypes, "text/markdown, text/html") {
		t.Fatalf("Accept %q does not prefer Markdown and HTML", acceptTypes)
	}
}

func TestBodySizeLimit(t *testing.T) {
	server := serve(t, "text/plain", strings.Repeat("a", maxBodyBytes)+"TAIL")
	result := runFetch(t, fetchInput(server.URL, maxBodyBytes-9, 0))
	if result.IsError {
		t.Fatalf("error result: %.300s", result.Content)
	}
	for _, want := range []string{"only the first 5 MiB were read", fmt.Sprintf("of %d.", maxBodyBytes), "\n\naaaaaaaaaa"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("result lacks %q:\n%.500s", want, result.Content)
		}
	}
	if strings.Contains(result.Content, "TAIL") || strings.Contains(result.Content, "More text remains") {
		t.Fatalf("result has text after the limit:\n%s", result.Content)
	}

	small := serve(t, "text/plain", strings.Repeat("a", 100))
	if result := runFetch(t, fetchInput(small.URL, 0, 0)); strings.Contains(result.Content, "Note:") {
		t.Fatalf("small body has a size notice:\n%s", result.Content)
	}
}

func TestPaging(t *testing.T) {
	ascii := serve(t, "text/plain", "abcdefghij")
	accented := serve(t, "text/plain", "éàüöß")
	cases := []struct {
		name             string
		server           *httptest.Server
		offset, maxChars int
		isError          bool
		want             []string
		unwanted         []string
	}{
		{"whole text", ascii, 0, 0, false, []string{"Characters 1-10 of 10.\n\nabcdefghij"}, []string{"More text"}},
		{"first slice", ascii, 1, 4, false, []string{"Characters 1-4 of 10.\n\nabcd\n\n", "with offset 5."}, []string{"abcde"}},
		{"middle slice", ascii, 3, 4, false, []string{"Characters 3-6 of 10.\n\ncdef\n\n", "with offset 7."}, []string{"cdefg"}},
		{"last slice", ascii, 7, 4, false, []string{"Characters 7-10 of 10.\n\nghij"}, []string{"More text"}},
		{"slice past end", ascii, 9, 100, false, []string{"Characters 9-10 of 10.\n\nij"}, []string{"More text"}},
		{"last character", ascii, 10, 1, false, []string{"Characters 10-10 of 10.\n\nj"}, []string{"More text"}},
		{"offset past end", ascii, 11, 0, true, []string{"offset 11 is past the end", "10 characters"}, nil},
		{"characters, not bytes", accented, 2, 2, false, []string{"Characters 2-3 of 5.\n\nàü\n\n", "with offset 4."}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := runFetch(t, fetchInput(test.server.URL, test.offset, test.maxChars))
			if result.IsError != test.isError {
				t.Fatalf("error %v, want %v:\n%s", result.IsError, test.isError, result.Content)
			}
			for _, want := range test.want {
				if !strings.Contains(result.Content, want) {
					t.Errorf("result lacks %q:\n%s", want, result.Content)
				}
			}
			for _, unwanted := range test.unwanted {
				if strings.Contains(result.Content, unwanted) {
					t.Errorf("result contains %q:\n%s", unwanted, result.Content)
				}
			}
		})
	}

	empty := serve(t, "text/plain", "")
	if result := runFetch(t, fetchInput(empty.URL, 0, 0)); result.IsError || !strings.HasSuffix(result.Content, "The page has no text.") {
		t.Fatalf("empty page result: %v %q", result.IsError, result.Content)
	}
}

func TestSliceByteLimit(t *testing.T) {
	// Each character has 4 bytes, so the byte limit ends the slice before
	// max_chars does.
	text := strings.Repeat("😀", maxCharsLimit)
	part, total, taken := slice(text, 1, maxCharsLimit)
	if total != maxCharsLimit || len(part) > maxSliceBytes || taken != maxSliceBytes/4 || !utf8.ValidString(part) {
		t.Fatalf("slice: total %d, %d bytes, %d characters", total, len(part), taken)
	}
}

func TestInvalidInput(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{`not json`, "invalid input"},
		{`{}`, "url is required"},
		{`{"url":5}`, "invalid input"},
		{`{"url":"ftp://example.com/x"}`, "http:// or https://"},
		{`{"url":"file:///etc/passwd"}`, "http:// or https://"},
		{`{"url":"example.com"}`, "http:// or https://"},
		{`{"url":"https://"}`, "no host"},
		{`{"url":"https://example.com/%zz"}`, "invalid input"},
		{`{"url":"https://example.com","offset":0}`, "offset must be 1 or more"},
		{`{"url":"https://example.com","max_chars":0}`, "max_chars must be from 1 to 200000"},
		{`{"url":"https://example.com","max_chars":200001}`, "max_chars must be from 1 to 200000"},
	}
	for _, test := range cases {
		t.Run(test.input, func(t *testing.T) {
			result := runFetch(t, json.RawMessage(test.input))
			if !result.IsError || !strings.HasPrefix(result.Content, "invalid input") || !strings.Contains(result.Content, test.want) {
				t.Fatalf("result = %v %q, want error with %q", result.IsError, result.Content, test.want)
			}
		})
	}
}

func TestNetworkErrorIsResult(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := server.URL
	server.Close()
	result := runFetch(t, fetchInput(address, 0, 0))
	if !result.IsError || !strings.Contains(result.Content, "web_fetch failed") {
		t.Fatalf("result = %v %q", result.IsError, result.Content)
	}
}

func TestContextEndsFetch(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/body" {
			// Send the headers, then stop in the middle of the body.
			writer.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(writer, "start")
			writer.(http.Flusher).Flush()
		}
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	for _, path := range []string{"/headers", "/body"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			result, err := FetchTool().Run(ctx, fetchInput(server.URL+path, 0, 0))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run = %+v, %v; want deadline error", result, err)
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("Run took %v after the deadline", elapsed)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchTool().Run(ctx, fetchInput(server.URL+"/headers", 0, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with a cancelled context = %v", err)
	}
}

func TestToolShape(t *testing.T) {
	tool := FetchTool()
	if tool.Name != "web_fetch" || tool.ReadOnly {
		t.Fatalf("tool %q read-only %v", tool.Name, tool.ReadOnly)
	}
	if !json.Valid(tool.InputSchema) {
		t.Fatal("input schema is not valid JSON")
	}
}
