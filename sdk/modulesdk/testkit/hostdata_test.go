package testkit

import (
	"slices"
	"testing"
)

func TestHostDataStore(t *testing.T) {
	d := NewHostData()
	d.StorePut("a/1", []byte("x"))
	d.StorePut("a/2", []byte("y"))
	d.StorePut("b/1", []byte("z"))
	keys := d.StoreKeys("a/")
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"a/1", "a/2"}) {
		t.Fatalf("StoreKeys = %v", keys)
	}
	if len(d.StoreKeys("")) != 3 || len(d.StoreKeys("a/1/longer")) != 0 {
		t.Fatal("prefix matching wrong")
	}
	d.StoreDelete("a/1")
	if _, ok := d.StoreGet("a/1"); ok {
		t.Fatal("deleted key still present")
	}
}

func TestHostDataPolicyWakesWatchers(t *testing.T) {
	d := NewHostData()
	if rev, data := d.Policy(); rev != 0 || data != nil {
		t.Fatalf("initial policy = %d %q", rev, data)
	}
	w := d.addPolicyWatcher()
	d.SetPolicy([]byte("one"))
	// The watcher channel holds one wake-up; a second change while it is
	// full must not block the setter.
	d.SetPolicy([]byte("two"))
	<-w
	if rev, data := d.Policy(); rev != 2 || string(data) != "two" {
		t.Fatalf("policy = %d %q", rev, data)
	}
}

func TestHostDataPublishFansOut(t *testing.T) {
	d := NewHostData()
	exact := d.subscribe([]string{"toy.started"})
	glob := d.subscribe([]string{"toy.*"})
	other := d.subscribe([]string{"other.*"})
	full := d.subscribe([]string{"toy.*"})
	for range cap(full.ch) {
		full.ch <- PublishedEvent{}
	}

	d.Publish("toy.started", []byte("hi"))
	if ev := <-exact.ch; ev.Topic != "toy.started" || string(ev.Data) != "hi" {
		t.Fatalf("exact subscriber got %+v", ev)
	}
	if ev := <-glob.ch; ev.Topic != "toy.started" {
		t.Fatalf("glob subscriber got %+v", ev)
	}
	if len(other.ch) != 0 {
		t.Fatal("non-matching subscriber received the event")
	}
	if evs := d.Events(); len(evs) != 1 || evs[0].At.IsZero() {
		t.Fatalf("recorded events = %+v", evs)
	}
}

func TestTopicMatches(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		topic    string
		want     bool
	}{
		{[]string{"a.b"}, "a.b", true},
		{[]string{"a.*"}, "a.b", true},
		{[]string{"a.*"}, "a.", true},
		{[]string{"a.*"}, "ab", false},
		{[]string{"*"}, "anything", false},
		{[]string{"x", "a.*"}, "a.c", true},
		{nil, "a", false},
	} {
		if got := topicMatches(c.patterns, c.topic); got != c.want {
			t.Errorf("topicMatches(%v, %q) = %v, want %v", c.patterns, c.topic, got, c.want)
		}
	}
}
