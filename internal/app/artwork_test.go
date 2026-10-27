package app

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
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
	c := newArtCache(func(func()) {})
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
	c := newArtCache(func(func()) {})
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
	c := newArtCache(func(func()) {})
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
	c := newArtCache(func(func()) {})
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
	c := newArtCache(func(f func()) { posted <- f })
	c.now = func() time.Time { return now }
	c.get("a", false)
	(<-posted)()
	now = now.Add(time.Second)
	if a := c.get("a", false); a == nil || !a.failed || fetches.Load() != 1 {
		t.Fatalf("asked again at once: %+v, %d fetches", a, fetches.Load())
	}
	offline = false
	now = now.Add(artRetry(1))
	c.get("a", false)
	(<-posted)()
	if a := c.get("a", false); a == nil || a.failed || fetches.Load() != 2 {
		t.Errorf("not loaded again: %+v, %d fetches", a, fetches.Load())
	}
}
