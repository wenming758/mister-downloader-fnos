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

// 缓存里的种子通常没有 tracker：补进去之后 info hash 必须不变，原来的 tracker 不能丢。
func TestTorrentWithTrackers(t *testing.T) {
	info := "d6:lengthi12e4:name4:test12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae"
	body := []byte("d4:info" + info + "e")
	want, err := torrentInfoHash(body)
	if err != nil {
		t.Fatalf("torrentInfoHash: %v", err)
	}
	out, err := torrentWithTrackers(body, []string{"https://a.example/announce", "udp://b.example:6969/announce"})
	if err != nil {
		t.Fatalf("torrentWithTrackers: %v", err)
	}
	got, err := torrentInfoHash(out)
	if err != nil || got != want {
		t.Fatalf("info hash changed: %s -> %s (%v)", want, got, err)
	}
	entries, err := btopLevel(out)
	if err != nil {
		t.Fatalf("btopLevel: %v", err)
	}
	trs := torrentTrackers(entries)
	if len(trs) != 2 || trs[0] != "https://a.example/announce" || trs[1] != "udp://b.example:6969/announce" {
		t.Fatalf("trackers = %v", trs)
	}
	again, err := torrentWithTrackers(out, []string{"https://a.example/announce"})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if string(again) != string(out) {
		t.Fatal("adding an existing tracker changed the torrent")
	}
}

func TestMagnetTrackers(t *testing.T) {
	m := "magnet:?xt=urn:btih:ae31be061ab6c298e785adfb92c3932c34a54013&tr=https%3A%2F%2F004430.xyz%2Fannounce&tr=udp%3A%2F%2Ftracker.example%3A1337%2Fannounce"
	got := magnetTrackers(m)
	want := []string{"https://004430.xyz/announce", "udp://tracker.example:1337/announce"}
	if len(got) != len(want) {
		t.Fatalf("trackers = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trackers = %v, want %v", got, want)
		}
	}
}
