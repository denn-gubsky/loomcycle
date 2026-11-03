// @loomcycle/code-editor (RFC FW) — one structured-text editor for loomcycle
// surfaces. Import the styles once: `import "@loomcycle/code-editor/styles.css"`.

export { CodeEditor } from "./CodeEditor";
export type { CodeEditorHandle, CodeEditorProps, EditorDiagnostic, EditorLanguage } from "./CodeEditor";
export { StructuredEditor } from "./StructuredEditor";
export type { PathDiagnostic, StructuredEditorHandle, StructuredEditorProps } from "./StructuredEditor";
export { applyEdit, applyExternal, initState, switchFormat } from "./structuredModel";
export type { StructuredState } from "./structuredModel";
export { formatValue, locate, parseJson, parseText, parseYaml, pathAt, positionAt, toJson, toYaml } from "./structured";
export type { Format, JsonObject, JsonPath, ParseError, ParseResult, TextPosition } from "./structured";
