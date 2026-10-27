package app

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"netease-music-mygo/internal/netease"
)

// An animated WebP cover, as some playlists have, shows its first frame,
// in the bitmap too, which decodes its data again once it drops its pixels.
func TestAnimatedWebPCover(t *testing.T) {
	b, err := os.ReadFile("testdata/animated.webp")
	if err != nil {
		t.Fatal(err)
	}
	bmp, img, err := decodeArt(b)
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 24 || r.Dy() != 24 {
		t.Errorf("first frame is %v, want 24×24", r)
	}
	if w, h := bmp.Size(); w != 24 || h != 24 {
		t.Errorf("bitmap is %d×%d, want 24×24", w, h)
	}
}

// Once the covers take more than the limit, those asked for least
// recently go, so that a long session does not keep every cover it
// showed; one asked for again stays, however long ago it loaded.
func TestArtCacheDropsLeastRecentCovers(t *testing.T) {
	now := time.Unix(1e9, 0)
	c := newArtCache(func(func()) {}, "")
	c.now, c.limit = func() time.Time { return now }, 100
	for _, key := range []string{"first", "second", "third"} {
		c.add(key, &artwork{used: now, size: 30})
		now = now.Add(time.Minute)
	}
	c.get("first", false)
	c.add("fourth", &artwork{used: now, size: 30})
	if _, ok := c.art["second"]; ok {
		t.Error("kept the cover asked for least recently over the limit")
	}
	for _, key := range []string{"first", "fourth"} {
		if _, ok := c.art[key]; !ok {
			t.Errorf("dropped %q, asked for last", key)
		}
	}
}

// Covers stay under the limit however long since they showed. They went
// after 5 minutes not asked for: going back to a page after a while on
// others, or to the window after a while away, loaded them again.
func TestArtCacheKeepsCoversUnderLimit(t *testing.T) {
	now := time.Unix(1e9, 0)
	c := newArtCache(func(func()) {}, "")
	c.now = func() time.Time { return now }
	page := &artwork{used: now, size: 100 << 10}
	c.add("page", page)
	// Kept too, so that asking for it fetches nothing.
	c.add("other", &artwork{used: now})
	for range 60 {
		now = now.Add(time.Minute)
		c.get("other", false)
	}
	if c.get("page", false) != page {
		t.Error("dropped a cover not asked for in an hour, under the limit")
	}
}

// A cover dropped while something still holds it comes back as it was.
// It loaded again: Now Playing's cover went while other pages showed,
// though Now Playing still held it, and flashed in once it opened again.
func TestArtCacheKeepsHeldCovers(t *testing.T) {
	now := time.Unix(1e9, 0)
	c := newArtCache(func(func()) {}, "")
	c.now, c.limit = func() time.Time { return now }, 60
	held := &artwork{used: now, size: 30}
	c.add("now playing", held)
	for _, key := range []string{"other", "another"} {
		now = now.Add(time.Minute)
		c.add(key, &artwork{used: now, size: 30})
	}
	if _, ok := c.art["now playing"]; ok {
		t.Fatal("kept a cover over the limit")
	}
	if got := c.get("now playing", false); got != held {
		t.Error("loaded again a cover something still held")
	}
	runtime.KeepAlive(held)
}

// The covers a page shows fetch together, not 8 at a time as they decode:
// far from the CDN each is a round trip most of a second long, and the
// wall's 40 took 3 seconds in rounds of 8.
func TestArtCacheFetchesCoversTogether(t *testing.T) {
	const n = 20
	started, release := make(chan struct{}, n), make(chan struct{})
	defer func(f func(string) ([]byte, error)) { fetchCover = f }(fetchCover)
	fetchCover = func(string) ([]byte, error) {
		started <- struct{}{}
		<-release
		return nil, errors.New("offline")
	}
	defer close(release)
	c := newArtCache(func(func()) {}, "")
	for i := range n {
		c.get(strconv.Itoa(i), false)
	}
	for i := range n {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d covers fetching at once", i, n)
		}
	}
}

// A cover that failed to load loads again after a while, as it shows. It
// didn't: one timeout marked it failed for as long as it showed, and its
// placeholder stayed.
func TestArtCacheRetriesFailedCover(t *testing.T) {
	cover, err := os.ReadFile("testdata/animated.webp")
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	offline := true
	defer func(f func(string) ([]byte, error)) { fetchCover = f }(fetchCover)
	fetchCover = func(string) ([]byte, error) {
		fetches.Add(1)
		if offline {
			return nil, errors.New("timeout")
		}
		return cover, nil
	}
	now := time.Unix(1e9, 0)
	posted := make(chan func(), 8)
	c := newArtCache(func(f func()) { posted <- f }, "")
	c.now = func() time.Time { return now }
	// The wait passes on c.now alone: a frame asked for after it, on the
	// wall clock, would come in posted in place of the cover.
	c.wake = func(time.Duration) {}
	c.get("a", false)
	(<-posted)()
	now = now.Add(time.Second)
	if a := c.get("a", false); a == nil || !a.failed() || fetches.Load() != 1 {
		t.Fatalf("asked again at once: %+v, %d fetches", a, fetches.Load())
	}
	offline = false
	now = now.Add(artRetry(1))
	c.get("a", false)
	(<-posted)()
	if a := c.get("a", false); a == nil || a.failed() || fetches.Load() != 2 {
		t.Errorf("not loaded again: %+v, %d fetches", a, fetches.Load())
	}
}

// A cover the CDN hasn't got, answering 404, isn't asked for again as it
// shows: it was, every minute, for as long as it showed.
func TestArtCacheGivesUpOnMissingCover(t *testing.T) {
	var fetches atomic.Int32
	defer func(f func(string) ([]byte, error)) { fetchCover = f }(fetchCover)
	fetchCover = func(string) ([]byte, error) {
		fetches.Add(1)
		return nil, netease.StatusError(http.StatusNotFound)
	}
	now := time.Unix(1e9, 0)
	posted := make(chan func(), 8)
	c := newArtCache(func(f func()) { posted <- f }, "")
	c.now = func() time.Time { return now }
	c.wake = func(time.Duration) { t.Error("asked for a frame to load it again") }
	c.get("a", false)
	(<-posted)()
	now = now.Add(time.Minute)
	if a := c.get("a", false); a == nil || !a.failed() || fetches.Load() != 1 {
		t.Errorf("asked again: %+v, %d fetches", a, fetches.Load())
	}
}

// A cover that loads as the covers kept on disk are cleared isn't kept: it
// made their folder again, and the cache cleared came back.
func TestArtCacheClearDropsLoadingCover(t *testing.T) {
	cover, err := os.ReadFile("testdata/animated.webp")
	if err != nil {
		t.Fatal(err)
	}
	fetched := make(chan struct{})
	defer func(f func(string) ([]byte, error)) { fetchCover = f }(fetchCover)
	fetchCover = func(string) ([]byte, error) {
		<-fetched
		return cover, nil
	}
	posted := make(chan func(), 8)
	c := newArtCache(func(f func()) { posted <- f }, filepath.Join(t.TempDir(), "covers"))
	c.get("a", false)
	c.clear()
	close(fetched)
	(<-posted)()
	if a := c.get("a", false); a == nil || a.failed() {
		t.Fatal("cover didn't load")
	}
	if _, err := os.Stat(c.dir); err == nil {
		t.Error("covers kept after they were cleared")
	}
}

// A cover loaded once shows from disk the next time the app opens, without
// the network; what came back but isn't a cover is not kept.
func TestArtCacheKeepsCoversOnDisk(t *testing.T) {
	cover, err := os.ReadFile("testdata/animated.webp")
	if err != nil {
		t.Fatal(err)
	}
	defer func(f func(string) ([]byte, error)) { fetchCover = f }(fetchCover)
	fetchCover = func(url string) ([]byte, error) {
		if url == "error" {
			return []byte("<html>"), nil
		}
		return cover, nil
	}
	dir := t.TempDir()
	load := func(c *artCache, url string) *artwork {
		posted := make(chan func(), 8)
		c.post = func(f func()) { posted <- f }
		c.get(url, false)
		(<-posted)()
		return c.get(url, false)
	}
	first := newArtCache(nil, dir)
	load(first, "cover")
	load(first, "error")
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("kept %d files, want the cover alone", len(entries))
	}
	fetchCover = func(string) ([]byte, error) { return nil, errors.New("offline") }
	if a := load(newArtCache(nil, dir), "cover"); a == nil || a.failed() {
		t.Error("cover kept last time didn't show offline")
	}
}

// The cover a song starts with, asked for by the page and the player at
// once, is fetched once, and the player's shows offline from disk. The
// player fetched its own, twice the download and none without the
// network.
func TestArtCacheSharesCoverWithPlayer(t *testing.T) {
	cover, err := os.ReadFile("testdata/animated.webp")
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	started, release := make(chan struct{}, 2), make(chan struct{})
	defer func(f func(string) ([]byte, error)) { fetchCover = f }(fetchCover)
	fetchCover = func(string) ([]byte, error) {
		fetches.Add(1)
		started <- struct{}{}
		<-release
		return cover, nil
	}
	posted := make(chan func(), 8)
	dir := t.TempDir()
	c := newArtCache(func(f func()) { posted <- f }, dir)
	c.get("cover", true)
	<-started
	got := make(chan []byte)
	go func() {
		b, _ := c.cover("cover")
		got <- b
	}()
	// The player's can't be seen waiting; it is let go once it waits, or
	// fetched its own.
	time.Sleep(50 * time.Millisecond)
	close(release)
	if b := <-got; !bytes.Equal(b, cover) {
		t.Error("player got a cover other than the page's")
	}
	(<-posted)()
	if n := fetches.Load(); n != 1 {
		t.Errorf("fetched %d times, want once", n)
	}
	fetchCover = func(string) ([]byte, error) { return nil, errors.New("offline") }
	if b, err := newArtCache(nil, dir).cover("cover"); err != nil || !bytes.Equal(b, cover) {
		t.Error("cover kept last time didn't show the player offline")
	}
}

// Past the limit, the covers on disk shown longest ago go first.
func TestPruneArt(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"oldest", "older", "newest"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(time.Duration(i-3) * time.Hour)
		os.Chtimes(path, at, at)
	}
	pruneArt(dir, 200)
	for name, want := range map[string]bool{"oldest": false, "older": true, "newest": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s kept %v, want %v", name, err == nil, want)
		}
	}
}
