package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureNotebook is a 2-cell .ipynb file used by every test.
const fixtureNotebook = `{
 "cells": [
  {"id": "aaaaaaaa", "cell_type": "code", "source": ["print(1)\n"], "outputs": [], "execution_count": null},
  {"id": "bbbbbbbb", "cell_type": "markdown", "source": ["# heading\n"]}
 ],
 "metadata": {"kernelspec": {"name": "python3"}},
 "nbformat": 4,
 "nbformat_minor": 5
}
`

func writeFixture(t *testing.T) (root, file string) {
	t.Helper()
	root = t.TempDir()
	file = filepath.Join(root, "nb.ipynb")
	if err := os.WriteFile(file, []byte(fixtureNotebook), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, file
}

// readNB reparses the file off disk for assertions.
func readNB(t *testing.T, path string) notebook {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var nb notebook
	if err := json.Unmarshal(raw, &nb); err != nil {
		t.Fatalf("parse-back: %v\nraw:\n%s", err, raw)
	}
	return nb
}

func TestNotebookEdit_ReplaceExistingCell(t *testing.T) {
	root, file := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","cell_id":"aaaaaaaa","source":"print(42)","mode":"replace"}`))
	if res.IsError {
		t.Fatalf("replace failed: %s", res.Text)
	}
	nb := readNB(t, file)
	if len(nb.Cells) != 2 {
		t.Fatalf("cell count changed: %d", len(nb.Cells))
	}
	if nb.Cells[0].ID != "aaaaaaaa" {
		t.Errorf("ordering changed: %v", nb.Cells)
	}
	if strings.Join(nb.Cells[0].Source, "") != "print(42)" {
		t.Errorf("source not replaced, got %q", nb.Cells[0].Source)
	}
	if nb.Cells[0].CellType != "code" {
		t.Errorf("cell_type unexpectedly changed to %q", nb.Cells[0].CellType)
	}
}

func TestNotebookEdit_InsertAfterCell(t *testing.T) {
	root, file := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","cell_id":"aaaaaaaa","source":"y = 2","mode":"insert"}`))
	if res.IsError {
		t.Fatalf("insert: %s", res.Text)
	}
	nb := readNB(t, file)
	if len(nb.Cells) != 3 {
		t.Fatalf("expected 3 cells, got %d", len(nb.Cells))
	}
	// New cell is at index 1 (after "aaaaaaaa").
	if nb.Cells[0].ID != "aaaaaaaa" || nb.Cells[2].ID != "bbbbbbbb" {
		t.Errorf("ordering wrong: %v %v %v", nb.Cells[0].ID, nb.Cells[1].ID, nb.Cells[2].ID)
	}
	if strings.Join(nb.Cells[1].Source, "") != "y = 2" {
		t.Errorf("source wrong: %q", nb.Cells[1].Source)
	}
	if nb.Cells[1].CellType != "code" {
		t.Errorf("default cell_type should be 'code', got %q", nb.Cells[1].CellType)
	}
	if nb.Cells[1].ID == "" {
		t.Errorf("expected minted id, got empty")
	}
}

func TestNotebookEdit_InsertAtStartWhenCellIDEmpty(t *testing.T) {
	root, file := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","source":"top","cell_type":"markdown","mode":"insert"}`))
	if res.IsError {
		t.Fatalf("insert at start: %s", res.Text)
	}
	nb := readNB(t, file)
	if len(nb.Cells) != 3 {
		t.Fatalf("expected 3 cells, got %d", len(nb.Cells))
	}
	if nb.Cells[0].CellType != "markdown" {
		t.Errorf("expected new first cell to be markdown, got %q", nb.Cells[0].CellType)
	}
	if strings.Join(nb.Cells[0].Source, "") != "top" {
		t.Errorf("first-cell source wrong: %q", nb.Cells[0].Source)
	}
	// Old aaaaaaaa is now at index 1, bbbbbbbb at index 2.
	if nb.Cells[1].ID != "aaaaaaaa" || nb.Cells[2].ID != "bbbbbbbb" {
		t.Errorf("existing cells shifted incorrectly: %v %v", nb.Cells[1].ID, nb.Cells[2].ID)
	}
}

func TestNotebookEdit_DeleteCell(t *testing.T) {
	root, file := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","cell_id":"aaaaaaaa","mode":"delete"}`))
	if res.IsError {
		t.Fatalf("delete: %s", res.Text)
	}
	nb := readNB(t, file)
	if len(nb.Cells) != 1 {
		t.Fatalf("expected 1 cell after delete, got %d", len(nb.Cells))
	}
	if nb.Cells[0].ID != "bbbbbbbb" {
		t.Errorf("wrong cell remained: %q", nb.Cells[0].ID)
	}
}

func TestNotebookEdit_CellNotFoundIsError(t *testing.T) {
	root, _ := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","cell_id":"nonexistent","source":"x","mode":"replace"}`))
	if !res.IsError {
		t.Errorf("expected error for missing cell, got %q", res.Text)
	}
}

func TestNotebookEdit_InvalidJSONIsError(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "bad.ipynb")
	if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"bad.ipynb","cell_id":"x","source":"y","mode":"replace"}`))
	if !res.IsError {
		t.Errorf("expected JSON parse error")
	}
}

func TestNotebookEdit_NonIpynbExtensionRejected(t *testing.T) {
	root := t.TempDir()
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"file.txt","cell_id":"x","source":"y","mode":"replace"}`))
	if !res.IsError {
		t.Errorf("non-.ipynb path should be rejected")
	}
}

func TestNotebookEdit_PathEscapeRejected(t *testing.T) {
	root, _ := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"/etc/passwd.ipynb","cell_id":"x","source":"y","mode":"replace"}`))
	if !res.IsError {
		t.Errorf("path outside root must refuse, got %q", res.Text)
	}
}

func TestNotebookEdit_MissingRootRefuses(t *testing.T) {
	n := &NotebookEdit{}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"x.ipynb","cell_id":"y","source":"z","mode":"replace"}`))
	if !res.IsError {
		t.Errorf("missing root must refuse")
	}
	if !strings.Contains(res.Text, "LOOMCYCLE_WRITE_ROOT") {
		t.Errorf("refusal should mention LOOMCYCLE_WRITE_ROOT, got %q", res.Text)
	}
}

func TestNotebookEdit_AtomicWriteLeavesNoTempfile(t *testing.T) {
	root, file := writeFixture(t)
	n := &NotebookEdit{Root: root}
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","cell_id":"aaaaaaaa","source":"q","mode":"replace"}`))
	if res.IsError {
		t.Fatalf("replace: %s", res.Text)
	}
	// Walk dir; no .loomcycle-nbedit-* should be left behind.
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".loomcycle-nbedit-") {
			t.Errorf("tempfile %q leaked", e.Name())
		}
	}
}

func TestNotebookEdit_PreservesUnaffectedCells(t *testing.T) {
	root, file := writeFixture(t)
	n := &NotebookEdit{Root: root}
	// Replace aaaaaaaa; bbbbbbbb must be untouched.
	res, _ := n.Execute(context.Background(), json.RawMessage(`{"file_path":"nb.ipynb","cell_id":"aaaaaaaa","source":"new","mode":"replace"}`))
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	nb := readNB(t, file)
	if nb.Cells[1].ID != "bbbbbbbb" {
		t.Errorf("bbbbbbbb missing or moved")
	}
	if strings.Join(nb.Cells[1].Source, "") != "# heading\n" {
		t.Errorf("bbbbbbbb source mutated: %q", nb.Cells[1].Source)
	}
	if nb.NBFormat != 4 || nb.NBFormatMinor != 5 {
		t.Errorf("notebook header changed: %v / %v", nb.NBFormat, nb.NBFormatMinor)
	}
}
