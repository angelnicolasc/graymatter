package providerhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

func newTestClient(t *testing.T, base string, template *http.Client) *Client {
	t.Helper()
	parsed, err := providerurl.Parse("anthropic", base)
	if err != nil {
		t.Fatal(err)
	}
	return New("anthropic", parsed, template)
}

func newTestRequest(t *testing.T, target, token string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader("body-"+token))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Api-Key", "key-"+token)
	request.Header.Set("X-Control", "preserved")
	return request
}

type testTransport func(*http.Request) (*http.Response, error)

func (transport testTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestOriginCanonicalization(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		same        bool
	}{
		{"http://EXAMPLE.test", "http://example.test:80", true},
		{"https://example.test:0443", "https://EXAMPLE.test", true},
		{"https://İ.EXAMPLE.test", "https://İ.example.test", true},
		{"http://İ.example.test", "http://i.example.test", false},
		{"http://i.example.test", "http://İ.example.test", false},
		{"http://İ.example.test", "http://xn--i-9bb.example.test", false},
		{"http://example.test.", "http://example.test", false},
		{"http://sub.example.test", "http://example.test", false},
		{"http://[::1]", "http://[0:0:0:0:0:0:0:1]:80", true},
		{"http://[::ffff:127.0.0.1]", "http://127.0.0.1", false},
		{"http://[fe80::1%25WiFi]", "http://[fe80:0::1%25WiFi]", true},
		{"http://[fe80::1%25WiFi]", "http://[fe80::1%25wifi]", false},
		{"http://example.test", "http://example.test:443", false},
		{"http://example.test:443", "https://example.test:443", false},
	} {
		left, _ := url.Parse(tc.left)
		right, _ := url.Parse(tc.right)
		a, aErr := urlOrigin(left)
		b, bErr := urlOrigin(right)
		if aErr != "" || bErr != "" || (a == b) != tc.same {
			t.Errorf("origins %q vs %q = (%+v %q), (%+v %q), same=%v; want %v", tc.left, tc.right, a, aErr, b, bErr, a == b, tc.same)
		}
	}
	input := string([]byte{0xff, 'A', 0xc3, 'Z', 0x00})
	want := string([]byte{0xff, 'a', 0xc3, 'z', 0x00})
	if got := foldASCII(input); got != want {
		t.Errorf("foldASCII bytes: got %x, want %x", got, want)
	}
}

func TestRedirectsKeepAuthBodyAndCallbackSnapshotsIsolated(t *testing.T) {
	type requestEvent struct{ method, uri, body, auth, key, control, cookie, proxy string }
	events := make(chan requestEvent, 4)
	location := make(chan string, 1)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		events <- requestEvent{r.Method, r.RequestURI, string(body), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Control"), r.Header.Get("Cookie"), r.Header.Get("Proxy-Authorization")}
		switch r.URL.Path {
		case "/start":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "fresh", Path: "/middle"})
			w.Header().Set("Location", <-location)
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/middle":
			w.Header().Set("Location", "final%23path?done=1")
			w.WriteHeader(http.StatusPermanentRedirect)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer fixture.Close()
	address := fixture.Listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	location <- "http://example.test:" + port + "/middle"
	transport := &http.Transport{DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		if foldASCII(target) != "example.test:"+port {
			return nil, fmt.Errorf("unexpected destination: %s", target)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	jar, _ := cookiejar.New(nil)
	// Mutate after the callback returns, while each redirected request is held
	// immediately before the real transport serializes it.
	type hopBarrier struct {
		transportReached chan struct{}
		mutated          chan struct{}
		reachedOnce      sync.Once
	}
	hops := []*hopBarrier{
		{transportReached: make(chan struct{}), mutated: make(chan struct{})},
		{transportReached: make(chan struct{}), mutated: make(chan struct{})},
	}
	finished := make(chan struct{})
	var mutations sync.WaitGroup
	var callbacks int
	template := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	template.Transport = testTransport(func(request *http.Request) (*http.Response, error) {
		var barrier *hopBarrier
		switch request.URL.Path {
		case "/middle":
			barrier = hops[0]
		case "/final#path":
			barrier = hops[1]
		}
		if barrier != nil {
			barrier.reachedOnce.Do(func() { close(barrier.transportReached) })
			select {
			case <-barrier.mutated:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return transport.RoundTrip(request)
	})
	template.CheckRedirect = func(observed *http.Request, via []*http.Request) error {
		if callbacks == len(hops) {
			return errors.New("unexpected hop")
		}
		barrier := hops[callbacks]
		callbacks++
		if observed.GetBody != nil || observed.Body != http.NoBody || observed.Response.Request != nil || observed.Response.Body != http.NoBody || observed.Context().Done() != nil || observed.Header.Get("Authorization") != "Bearer synthetic" || observed.Header.Get("X-Api-Key") != "key-synthetic" {
			t.Error("callback did not receive the isolated request metadata and intended authentication")
		}
		mutations.Add(1)
		go func() {
			defer mutations.Done()
			select {
			case <-barrier.transportReached:
			case <-finished:
				return
			}
			defer close(barrier.mutated)
			if payload, _ := io.ReadAll(observed.Body); len(payload) != 0 {
				t.Error("callback could read the real body")
			}
			_ = observed.Body.Close()
			observed.URL.Opaque = "//foreign.test/leak"
			observed.URL.User = url.User("untrusted")
			observed.Method = http.MethodGet
			observed.Host = "foreign.test"
			observed.RequestURI = "http://foreign.test/leak"
			observed.ContentLength = 1
			observed.TransferEncoding = []string{"chunked"}
			observed.Body = io.NopCloser(strings.NewReader("foreign"))
			observed.GetBody = func() (io.ReadCloser, error) { return observed.Body, nil }
			observed.Header["Authorization"][0] = "changed"
			observed.Header.Set("X-Control", "changed")
			observed.Response.Header.Set("Location", "http://foreign.test/leak")
			via[0].URL.Host = "foreign.test"
			via[0].Header.Set("X-Api-Key", "changed")
			via[0] = nil
		}()
		return nil
	}
	base := "http://EXAMPLE.test:" + port
	client := newTestClient(t, base, template)
	request := newTestRequest(t, base+"/start", "synthetic")
	request.Header.Set("Proxy-Authorization", "proxy-canary")
	response, err := client.Do(request)
	close(finished)
	mutations.Wait()
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || len(events) != 3 || callbacks != 2 {
		t.Fatalf("status=%d requests=%d callbacks=%d", response.StatusCode, len(events), callbacks)
	}
	for i, uri := range []string{"/start", "/middle", "/final%23path?done=1"} {
		got := <-events
		if got.method != http.MethodPost || got.uri != uri || got.body != "body-synthetic" || got.auth != "Bearer synthetic" || got.key != "key-synthetic" || got.control != "preserved" {
			t.Fatalf("request[%d] = %+v", i, got)
		}
		if (i == 1 && got.cookie != "session=fresh") || (i != 1 && got.cookie != "") || (i > 0 && got.proxy != "") {
			t.Fatalf("unexpected restored header: request[%d] = %+v", i, got)
		}
	}
}

func TestRejectedRedirectDoesNotSendToSecondOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, location, reason string
		status                 int
		calls                  int
		veto                   error
	}{
		{"302 converts post", "/local", "status", 302, 1, nil},
		{"301 converts post", "/local", "status", 301, 1, nil},
		{"303 converts post", "/local", "status", 303, 1, nil},
		{"cross-origin port", "B", "origin", 307, 1, nil},
		{"empty fragment", "/local#", "fragment", 307, 1, nil},
		{"fragment", "/local#private-canary", "fragment", 308, 1, nil},
		{"userinfo", "USER", "url_shape", 307, 1, nil},
		{"terminal without location", "", "terminal_status", 307, 1, nil},
		{"terminal other 3xx", "/local", "terminal_status", 300, 1, nil},
		{"callback error", "/local", "callback_veto", 307, 1, errors.New("callback-canary")},
		{"callback sentinel", "/local", "callback_veto", 308, 1, http.ErrUseLastResponse},
		{"loop", "/local", "limit", 307, 10, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming, foreign, callbackCalls atomic.Int32
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				foreign.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer second.Close()
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				incoming.Add(1)
				if tc.location == "B" {
					w.Header().Set("Location", second.URL+"/canary")
				} else if tc.location == "USER" {
					w.Header().Set("Location", "http://key-canary@"+r.Host+"/local")
				} else if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.Header().Set("X-Should-Retry", "true")
				w.WriteHeader(tc.status)
			}))
			defer first.Close()
			first.Client().CheckRedirect = func(*http.Request, []*http.Request) error { callbackCalls.Add(1); return tc.veto }
			client := newTestClient(t, first.URL, first.Client())
			response, err := client.Do(newTestRequest(t, first.URL+"/private-path-canary", "key-canary"))
			var blocked *RedirectBlockedError
			if !errors.As(err, &blocked) || blocked.Reason != tc.reason || blocked.Provider != "anthropic" || strings.Contains(err.Error(), "canary") {
				t.Fatalf("unexpected error: %v", err)
			}
			if response == nil || response.StatusCode != tc.status || response.Body != http.NoBody || response.Header.Get("X-Should-Retry") != "true" {
				t.Fatalf("missing retry response metadata: %+v", response)
			}
			wantCallbacks := tc.calls - 1
			if tc.veto != nil {
				wantCallbacks = 1
				if !errors.Is(err, tc.veto) {
					t.Fatalf("callback cause was lost: %v", err)
				}
			}
			if incoming.Load() != int32(tc.calls) || foreign.Load() != 0 || callbackCalls.Load() != int32(wantCallbacks) {
				t.Fatalf("requests=%d second=%d callbacks=%d", incoming.Load(), foreign.Load(), callbackCalls.Load())
			}
		})
	}
}

func TestAuthCaseVariantsCopiedDeterministically(t *testing.T) {
	initial := http.Header{"authorization": {"lower", "lower-2"}, "Authorization": {"canonical"}, "AUTHORIZATION": {"upper"}, "x-api-key": {}}
	auth := captureAuth(initial)
	initial["authorization"][0] = "mutated"
	redirect := http.Header{"AUTHORIZATION": {"wrong"}, "Cookie": {"jar"}, "Proxy-Authorization": {"proxy"}}
	auth.restore(redirect)
	want := []string{"upper", "canonical", "lower", "lower-2"}
	_, emptyKeyPresent := redirect["X-Api-Key"]
	if !reflect.DeepEqual(redirect.Values("Authorization"), want) || redirect["AUTHORIZATION"] != nil || !emptyKeyPresent || len(redirect["X-Api-Key"]) != 0 || redirect.Get("Cookie") != "jar" || redirect.Get("Proxy-Authorization") != "proxy" {
		t.Fatalf("unexpected reconstructed headers: %+v", redirect)
	}
	redirect["Authorization"][2] = "mutated"
	auth.restore(redirect)
	if !reflect.DeepEqual(redirect.Values("Authorization"), want) {
		t.Fatalf("the restored list aliases its source: %+v", redirect)
	}
	captureAuth(nil).restore(redirect)
	for _, name := range authNames {
		if _, exists := redirect[name]; exists {
			t.Fatalf("header %s resurrected from another request", name)
		}
	}
}

func TestUnicodeRedirectsNeverDialSecondOrigin(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/reverse=%t", status, reverse), func(t *testing.T) {
				selected, other := "İ.example.test", "i.example.test"
				selectedWire, otherWire := "xn--i-9bb.example.test:8080", "i.example.test:8080"
				// net/http uses the lookup IDNA profile for the dial target
				// and plain Punycode for the serialized Host header.
				selectedHeader := "xn--bfa.example.test:8080"
				if reverse {
					selected, other = other, "%C4%B0.example.test"
					selectedWire, otherWire = otherWire, selectedWire
					selectedHeader = selectedWire
				}
				var firstCalls, otherCalls atomic.Int32
				first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					firstCalls.Add(1)
					payload, _ := io.ReadAll(r.Body)
					if r.Method != http.MethodPost || r.Host != selectedHeader || r.Header.Get("Authorization") != "Bearer unicode" || r.Header.Get("X-Api-Key") != "key-unicode" || string(payload) != "body-unicode" {
						t.Errorf("initial request authority=%q; expected %q, with the original method, authentication and body", r.Host, selectedHeader)
					}
					w.Header().Set("Location", "http://"+other+":8080/next")
					w.WriteHeader(status)
				}))
				defer first.Close()
				second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					otherCalls.Add(1)
					w.WriteHeader(http.StatusOK)
				}))
				defer second.Close()
				dials := make(chan string, 4)
				transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					dials <- address
					switch address {
					case selectedWire:
						return (&net.Dialer{}).DialContext(ctx, network, first.Listener.Addr().String())
					case otherWire:
						return (&net.Dialer{}).DialContext(ctx, network, second.Listener.Addr().String())
					default:
						return nil, errors.New("refusing to dial outside the two loopback fixtures")
					}
				}}
				defer transport.CloseIdleConnections()
				base := "http://" + selected + ":8080"
				client := newTestClient(t, base, &http.Client{Transport: transport, Timeout: 3 * time.Second})
				_, err := client.Do(newTestRequest(t, base+"/start", "unicode"))
				var blocked *RedirectBlockedError
				if !errors.As(err, &blocked) || blocked.Reason != "origin" || firstCalls.Load() != 1 || otherCalls.Load() != 0 || len(dials) != 1 || <-dials != selectedWire {
					t.Fatalf("unexpected traffic: first=%d second=%d pending dials=%d error=%v", firstCalls.Load(), otherCalls.Load(), len(dials), err)
				}
			})
		}
	}
}

func TestInvalidRequestAndMalformedLocationErrorsAreSanitized(t *testing.T) {
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", "http://[malformed-location-canary")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer fixture.Close()
	client := newTestClient(t, fixture.URL, fixture.Client())
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Host = "alternate-host-canary" },
		func(r *http.Request) { r.Header["host"] = []string{"alternate-header-canary"} },
		func(r *http.Request) { r.URL.Opaque = "//alternate-opaque-canary" },
		func(r *http.Request) { r.URL.RawPath = "/not-the-path-canary" },
		func(r *http.Request) { r.URL.User = url.User("userinfo-canary") },
		func(r *http.Request) { r.RequestURI = "/alternate-uri-canary" },
		func(r *http.Request) { r.URL.OmitHost = true },
		func(r *http.Request) { r.GetBody = nil },
		func(r *http.Request) { r.Body = http.NoBody },
		func(r *http.Request) { r.ContentLength = -1 },
		func(r *http.Request) { r.TransferEncoding = []string{"chunked"} },
		func(r *http.Request) { r.Header["content-length"] = []string{"-1"} },
		func(r *http.Request) { r.Trailer = http.Header{"X-Trailer": {"trailer-canary"}} },
	} {
		request := newTestRequest(t, fixture.URL+"/path-canary", "synthetic")
		mutate(request)
		_, err := client.Do(request)
		var blocked *RedirectBlockedError
		if !errors.As(err, &blocked) || strings.Contains(err.Error(), "canary") || requests.Load() != 0 {
			t.Fatalf("invalid request reached the provider or leaked diagnostics: requests=%d err=%v", requests.Load(), err)
		}
	}
	response, err := client.Do(newTestRequest(t, fixture.URL+"/path-canary", "synthetic"))
	var safe *TransportError
	var cause *url.Error
	if response != nil || requests.Load() != 1 || !errors.As(err, &safe) || !errors.As(err, &cause) || strings.Contains(err.Error(), "canary") {
		t.Fatalf("the malformed Location error was not sanitized: response=%v requests=%d err=%v", response, requests.Load(), err)
	}
}

func TestConcurrentRedirectsKeepTheirOwnAuth(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/start/") {
			w.Header().Set("Location", strings.Replace(r.URL.Path, "/start/", "/end/", 1))
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		token := strings.TrimPrefix(r.URL.Path, "/end/")
		payload, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Api-Key") != "key-"+token || string(payload) != "body-"+token {
			t.Errorf("credentials or payload from a different request reached %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fixture.Close()
	client := newTestClient(t, fixture.URL, fixture.Client())
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		token := fmt.Sprintf("synthetic-%d", i)
		request := newTestRequest(t, fixture.URL+"/start/"+token, token)
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := client.Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Errorf("unexpected status %d", response.StatusCode)
			}
		}()
	}
	group.Wait()
}

func TestIPv6LoopbackRequest(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	fixture := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != listener.Addr().String() || r.URL.Path != "/prefix/messages" || r.Method != http.MethodPost {
			t.Error("unexpected IPv6 request authority, path or method")
		}
		w.WriteHeader(http.StatusOK)
	}))
	_ = fixture.Listener.Close()
	fixture.Listener = listener
	fixture.Start()
	defer fixture.Close()
	client := newTestClient(t, fixture.URL+"/prefix", fixture.Client())
	response, err := client.Do(newTestRequest(t, fixture.URL+"/prefix/messages", "synthetic"))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%v error=%v", response, err)
	}
	_ = response.Body.Close()
}
