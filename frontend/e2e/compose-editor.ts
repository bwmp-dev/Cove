import type { EditorView } from "@codemirror/view";
import { expect, type Locator } from "@playwright/test";

// Locator.fill types into CodeMirror's contenteditable through the browser's editing commands,
// which turn indentation spaces into non-breaking spaces. Replacing the document through the
// editor's own view keeps the exact text and still reaches onChange like any other edit.
// The view lookup mirrors EditorView.findFromDOM, which the page bundle does not expose.
export async function fillComposeEditor(editor: Locator, text: string) {
  const document = await editor.evaluate((element, insert) => {
    const content = element.closest(".cm-editor")?.querySelector<HTMLElement & { cmTile?: { root?: { view?: EditorView } } }>(".cm-content");
    const view = content?.cmTile?.root?.view;
    if (!view) throw new Error("Compose editor view not found");
    view.focus();
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert }, selection: { anchor: insert.length }, userEvent: "input" });
    return view.state.doc.toString();
  }, text);
  expect(document).toBe(text);
  expect(document).not.toContain(" ");
}
