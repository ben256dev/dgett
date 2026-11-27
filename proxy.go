package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const proxySock = "/run/dgett/watch.sock"

const eventsPath = "/_dgett/events"

// reloadScript is injected into every HTML page of a watched deploy. Each
// event first fires a cancelable window event ("dgett:building",
// "dgett:builderror", "dgett:reload", "dgett:ok", detail = data); an app that
// calls preventDefault() replaces dgett's default for it, including the
// reload itself. building's data is {"started": unix ms, "estimate": ms of the
// last rebuild, 0 if none yet}.
const reloadScript = `<script>(()=>{
const e=new EventSource("` + eventsPath + `");
const fire=(t,d)=>dispatchEvent(new CustomEvent("dgett:"+t,{detail:d,cancelable:true}));
const rm=id=>{const n=document.getElementById(id);if(n)n.remove()};
const bar=()=>{if(window.top!==window||document.getElementById("_dgett_bar"))return;
const b=document.createElement("div");b.id="_dgett_bar";
b.style.cssText="position:fixed;left:0;right:0;top:0;height:3px;z-index:2147483647;pointer-events:none;background:linear-gradient(90deg,transparent,#999,transparent) no-repeat;background-size:40% 100%;animation:_dgett_slide 1.2s linear infinite";
const s=document.createElement("style");s.textContent="@keyframes _dgett_slide{from{background-position:-70% 0}to{background-position:170% 0}}";
b.appendChild(s);document.body.appendChild(b)};
e.addEventListener("building",m=>{if(fire("building",m.data))bar()});
e.addEventListener("reload",m=>{if(fire("reload",m.data))location.reload()});
e.addEventListener("builderror",m=>{rm("_dgett_bar");if(!fire("builderror",m.data))return;let b=document.getElementById("_dgett");
if(!b){b=document.createElement("pre");b.id="_dgett";
b.style.cssText="position:fixed;left:0;right:0;bottom:0;max-height:40vh;overflow:auto;margin:0;padding:8px;background:#300;color:#fdd;font:12px monospace;z-index:2147483647;white-space:pre-wrap";
document.body.appendChild(b)}b.textContent=m.data});
e.addEventListener("ok",m=>{rm("_dgett_bar");rm("_dgett");fire("ok",m.data)});
})()</script>`

// hub fans out live reload events to every open page of one watched deploy.
type hub struct {
	mu   sync.Mutex
	subs map[chan [2]string]struct{}
	// last is the latest building or builderror event, replayed to pages
	// that connect while it is still current.
	last [2]string
}

func (h *hub) send(event, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if event == "building" || event == "builderror" {
		h.last = [2]string{event, data}
	} else {
		h.last = [2]string{}
	}
	for c := range h.subs {
		select {
		case c <- [2]string{event, data}:
		default:
		}
	}
}

func (h *hub) subscribe() (chan [2]string, [2]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan [2]string]struct{}{}
	}
	c := make(chan [2]string, 4)
	h.subs[c] = struct{}{}
	return c, h.last
}

func (h *hub) unsubscribe(c chan [2]string) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

func writeEvent(w io.Writer, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

// backends maps the per-generation upstream host the proxy dials to that
// generation's socket, so pooled connections never cross generations.
var backends sync.Map

func watchedDeploy(host string) *Deploy {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	mu.Lock()
	defer mu.Unlock()
	r := routes[host]
	if r == nil {
		return nil
	}
	if d := deploys[r.commit]; d != nil && d.w != nil {
		return d
	}
	return nil
}

func serveEvents(w http.ResponseWriter, r *http.Request, d *Deploy) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c, last := d.w.hub.subscribe()
	defer d.w.hub.unsubscribe(c)
	if last[0] != "" {
		writeEvent(w, last[0], last[1])
	}
	f.Flush()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.w.quit:
			return
		case ev := <-c:
			writeEvent(w, ev[0], ev[1])
			f.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			f.Flush()
		}
	}
}

func injectReload(resp *http.Response) error {
	if resp.Request.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if i := bytes.LastIndex(bytes.ToLower(body), []byte("</body>")); i >= 0 {
		body = append(body[:i:i], append([]byte(reloadScript), body[i:]...)...)
	} else {
		body = append(body, reloadScript...)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func newProxy() http.Handler {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			sock, ok := backends.Load(host)
			if !ok {
				return nil, fmt.Errorf("no backend for %s", host)
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock.(string))
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
	}
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			d := r.Context().Value(deployKey{}).(*Deploy)
			mu.Lock()
			sock := d.Socket
			mu.Unlock()
			upstream := strings.TrimSuffix(filepath.Base(sock), ".sock") + "." + d.Commit
			backends.Store(upstream, sock)
			r.URL.Scheme = "http"
			r.URL.Host = upstream
			r.Header.Del("Accept-Encoding")
		},
		Transport:      transport,
		FlushInterval:  -1,
		ModifyResponse: injectReload,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "dgett: app unavailable: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := watchedDeploy(r.Host)
		if d == nil {
			http.Error(w, "dgett: no watched deploy for "+r.Host, http.StatusNotFound)
			return
		}
		if r.URL.Path == eventsPath {
			serveEvents(w, r, d)
			return
		}
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deployKey{}, d)))
	})
}

type deployKey struct{}

func serveProxy() {
	os.Remove(proxySock)
	ln, err := net.Listen("unix", proxySock)
	if err != nil {
		log.Fatal("listen ", proxySock, ": ", err)
	}
	if err := os.Chown(proxySock, -1, int(appCred.Gid)); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(proxySock, 0o660); err != nil {
		log.Fatal(err)
	}
	go func() {
		log.Fatal(http.Serve(ln, newProxy()))
	}()
}
