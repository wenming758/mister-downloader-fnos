package main

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

func TestTorrentInfoHash(t *testing.T) {
	info := "d6:lengthi12e4:name4:test12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae"
	body := []byte("d8:announce14:http://tracker4:info" + info + "e")
	sum := sha1.Sum([]byte(info))
	want := hex.EncodeToString(sum[:])
	got, err := torrentInfoHash(body)
	if err != nil {
		t.Fatalf("torrentInfoHash: %v", err)
	}
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestTorrentInfoHashRejectsGarbage(t *testing.T) {
	for _, body := range []string{"", "not a torrent", "d4:infoi1ee"} {
		if _, err := torrentInfoHash([]byte(body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestMagnetInfoHash(t *testing.T) {
	hexHash := "ae31be061ab6c298e785adfb92c3932c34a54013"
	got, err := magnetInfoHash("magnet:?xt=urn:btih:" + strings.ToUpper(hexHash) + "&dn=test")
	if err != nil || got != hexHash {
		t.Fatalf("hex magnet: %q %v", got, err)
	}
	b32 := magnetBase32(hexHash)
	got, err = magnetInfoHash("magnet:?xt=urn:btih:" + b32)
	if err != nil || got != hexHash {
		t.Fatalf("base32 magnet: %q %v (base32 %s)", got, err, b32)
	}
	if _, err := magnetInfoHash("magnet:?xt=urn:sha1:abc"); err == nil {
		t.Fatal("accepted a magnet without btih")
	}
}

func TestWithPublicTrackers(t *testing.T) {
	base := "magnet:?xt=urn:btih:ae31be061ab6c298e785adfb92c3932c34a54013"
	out := withPublicTrackers(base)
	if strings.Count(out, "tr=") != len(publicTrackers) {
		t.Fatalf("expected %d trackers in %s", len(publicTrackers), out)
	}
	again := withPublicTrackers(out)
	if strings.Count(again, "tr=") != len(publicTrackers) {
		t.Fatalf("trackers duplicated: %s", again)
	}
}
