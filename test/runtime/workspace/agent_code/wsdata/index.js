// wsdata — the data pass (RFC GW §11, P2) from a code-js agent over the real
// loop, on the runtime's local worker. It moves every file by reference, so
// no file's bytes reach a tool result:
//   1. open a workspace and make a Document;
//   2. File render the Document into the workspace as Markdown;
//   3. write a Python script into the workspace and run it: it rewrites the
//      Markdown into out/result.md, adding a marker line it builds at run
//      time (so the marker's text appears in no tool input either), and
//      prints the workspace's directory name, which is its id;
//   4. File import out/result.md as a new Document (provenance: the
//      workspace, its id, this run);
//   5. File render that Document into the volume, as the format the driver
//      names in the prompt (pdf where Typst is installed, else docx);
//   6. File copy a small PDF from the volume into the workspace and File
//      extract it into the same workspace (out/extracted/), then File stat
//      the Markdown;
//   7. File copy the out/ folder, the extraction with it, into the volume,
//      and File rm one file.
// The run's end releases the workspace; the driver checks it is gone.
function run(input) {
  var fmt = String(input.prompt || "").trim() === "docx" ? "docx" : "pdf";
  var W = "workspace:data:";
  var open = JSON.parse(Workspace({ op: "open", name: "data", toolchain: "python" }));

  var src = JSON.parse(Document({ op: "import_md", scope: "user",
    markdown: "# Data pass source\n\n## Findings\n\nThe workspace keeps the files.\n" }));
  var r1 = JSON.parse(File({ op: "render", document: src.document_id, format: "md", to: W + "in/source.md" }));

  Workspace({ op: "write", name: "data", path: "rewrite.py", content:
    "import os\n" +
    "marker = 'DATAPASS' + '_REWRITTEN_' + str(6 * 7)\n" +
    "text = open('in/source.md', encoding='utf-8').read()\n" +
    "os.makedirs('out', exist_ok=True)\n" +
    "with open('out/result.md', 'w', encoding='utf-8') as f:\n" +
    "    f.write(text.replace('# Data pass source', '# Data pass result', 1).rstrip() + '\\n\\n## Rewritten\\n\\n' + marker + '\\n')\n" +
    "with open('out/notes.txt', 'w') as f:\n" +
    "    f.write('scratch\\n')\n" +
    "print('ws=' + os.path.basename(os.getcwd()))\n" });
  var ex = JSON.parse(Workspace({ op: "exec", name: "data", command: "python3 rewrite.py" }));
  var wsDir = String(ex.output).trim().replace(/^ws=/, "");

  var imp = JSON.parse(File({ op: "import", path: W + "out/result.md", scope: "user", doc_path: "/datapass/result" }));
  var p = imp.provenance || {};
  var tags = imp.tags || [];
  var r2 = JSON.parse(File({ op: "render", document: imp.document_id, format: fmt, to: "volume:work:result." + fmt }));

  var cp = JSON.parse(File({ op: "copy", from: "volume:work:fixture.pdf", to: W + "pdf/" }));
  var xt = JSON.parse(File({ op: "extract", path: W + "pdf/fixture.pdf", to: W + "out/extracted/" }));
  var st = JSON.parse(File({ op: "stat", path: W + "out/extracted/fixture.md" }));

  var keep = JSON.parse(File({ op: "copy", from: W + "out", to: "volume:work:kept/" }));
  var rm = JSON.parse(File({ op: "rm", path: W + "out/notes.txt" }));

  return { final_text:
    "state=" + open.state +
    " exit=" + ex.exit_code +
    " ws_dir=" + wsDir +
    " render_md=" + r1.ref +
    " captured_from=" + p.captured_from +
    " workspace_id=" + p.workspace_id +
    " run_id=" + p.run_id +
    " tag_ws=" + (tags.indexOf("source/workspace") >= 0) +
    " import_doc=" + (imp.document_id ? "yes" : "no") +
    " render_out=" + r2.ref +
    " copied_in=" + (cp.ref || cp.to) +
    " extracted=" + xt.ref +
    " extracted_size=" + st.size +
    " kept=" + keep.to + "/" + keep.files +
    " removed=" + rm.removed };
}
