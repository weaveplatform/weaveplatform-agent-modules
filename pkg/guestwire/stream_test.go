package guestwire

import (
	"bytes"
	"testing"
)

func TestAssemblerReassemblesInOrder(t *testing.T) {
	var a StreamAssembler
	var got bytes.Buffer
	chunks := []Chunk{
		{StreamID: "s", Seq: 0, Data: []byte("hello ")},
		{StreamID: "s", Seq: 1, Data: []byte("world")},
		{StreamID: "s", Seq: 2, EOF: true},
	}
	for _, c := range chunks {
		data, err := a.Accept(c)
		if err != nil {
			t.Fatalf("chunk %d: %v", c.Seq, err)
		}
		got.Write(data)
	}
	if got.String() != "hello world" {
		t.Fatalf("reassembled %q", got.String())
	}
	if !a.Done() {
		t.Fatal("EOF chunk did not finish the stream")
	}
	if err := a.Err(); err != nil {
		t.Fatalf("clean stream reported %v", err)
	}
}

// The whole point of Seq: a dropped chunk must be an error, because the
// alternative is a file that transfers "successfully" with a hole in it.
func TestAssemblerRejectsAGap(t *testing.T) {
	var a StreamAssembler
	if _, err := a.Accept(Chunk{StreamID: "s", Seq: 0, Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	_, err := a.Accept(Chunk{StreamID: "s", Seq: 2, Data: []byte("c")})
	if err == nil {
		t.Fatal("a missing chunk 1 was accepted — truncation would be silent")
	}
}

// A stream that stops without EOF must not look complete. This is what stops a
// dead channel mid-transfer from being read as success.
func TestAssemblerIsNotDoneWithoutEOF(t *testing.T) {
	var a StreamAssembler
	if _, err := a.Accept(Chunk{StreamID: "s", Seq: 0, Data: []byte("partial")}); err != nil {
		t.Fatal(err)
	}
	if a.Done() {
		t.Fatal("stream reported done without an EOF chunk")
	}
}

func TestAssemblerRejectsChunkAfterEOF(t *testing.T) {
	var a StreamAssembler
	if _, err := a.Accept(Chunk{StreamID: "s", Seq: 0, EOF: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Accept(Chunk{StreamID: "s", Seq: 1, Data: []byte("late")}); err == nil {
		t.Fatal("a chunk after EOF was accepted")
	}
}

func TestAssemblerSurfacesProducerError(t *testing.T) {
	var a StreamAssembler
	if _, err := a.Accept(Chunk{StreamID: "s", Seq: 0, Data: []byte("some")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Accept(
		Chunk{StreamID: "s", Seq: 1, EOF: true, Err: "disk went away"},
	); err != nil {
		t.Fatal(err)
	}
	if !a.Done() {
		t.Fatal("an errored stream is still finished")
	}
	if err := a.Err(); err == nil {
		t.Fatal("producer error was swallowed")
	}
}

func TestAssemblerRejectsOversizedChunk(t *testing.T) {
	var a StreamAssembler
	_, err := a.Accept(Chunk{StreamID: "s", Seq: 0, Data: make([]byte, MaxChunkBytes+1)})
	if err == nil {
		t.Fatal("an oversized chunk was accepted")
	}
}
