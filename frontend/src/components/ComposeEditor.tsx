import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { yaml } from "@codemirror/lang-yaml";
import { bracketMatching, HighlightStyle, indentOnInput, indentUnit, syntaxHighlighting } from "@codemirror/language";
import { Annotation, Compartment, EditorState, Transaction } from "@codemirror/state";
import { drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars, keymap, lineNumbers, placeholder as placeholderText } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { TextAlignStart } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { formatCompose } from "../lib/compose-format";
import { Button } from "./ui";

const theme = EditorView.theme(
  {
    "&": { height: "100%", backgroundColor: "var(--color-panel)", color: "var(--color-fg)", fontSize: "12px" },
    "&.cm-focused": { outline: "none" },
    ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.625", overflow: "auto" },
    ".cm-content": { padding: "6px 0", caretColor: "var(--color-fg)" },
    ".cm-line": { padding: "0 10px 0 6px" },
    ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--color-fg)" },
    "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection": { backgroundColor: "var(--color-info-line)" },
    ".cm-activeLine": { backgroundColor: "transparent" },
    "&.cm-focused .cm-activeLine": { backgroundColor: "var(--color-surface)" },
    ".cm-gutters": { backgroundColor: "var(--color-panel)", color: "var(--color-fg-subtle)", border: "none", borderRight: "1px solid var(--color-line)" },
    ".cm-activeLineGutter": { backgroundColor: "transparent" },
    "&.cm-focused .cm-activeLineGutter": { backgroundColor: "var(--color-surface)", color: "var(--color-fg-muted)" },
    ".cm-lineNumbers .cm-gutterElement": { padding: "0 6px 0 10px", minWidth: "32px" },
    ".cm-placeholder": { color: "var(--color-fg-subtle)" },
    ".cm-content[contenteditable=false]": { opacity: "0.6" },
    "&.cm-focused .cm-matchingBracket": { backgroundColor: "var(--color-raised)", outline: "1px solid var(--color-line-strong)" },
  },
  { dark: true },
);

// Marks changes that come from props, so they are neither reported back nor undoable.
const external = Annotation.define<boolean>();

const highlighting = HighlightStyle.define([
  { tag: tags.definition(tags.propertyName), color: "var(--color-info)" },
  { tag: [tags.string, tags.special(tags.string)], color: "var(--color-ok)" },
  { tag: [tags.number, tags.bool, tags.null], color: "var(--color-warn)" },
  { tag: [tags.labelName, tags.typeName, tags.keyword], color: "var(--color-bad)" },
  { tag: tags.comment, color: "var(--color-fg-subtle)", fontStyle: "italic" },
  { tag: [tags.punctuation, tags.separator, tags.squareBracket, tags.brace], color: "var(--color-fg-muted)" },
]);

interface ComposeEditorProps {
  id: string;
  labelId: string;
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
  invalid: boolean;
  describedBy?: string;
}

function contentAttributes(id: string, labelId: string, invalid: boolean, describedBy?: string) {
  const attributes: Record<string, string> = { id, "aria-labelledby": labelId, autocapitalize: "none", autocorrect: "off" };
  if (invalid) attributes["aria-invalid"] = "true";
  if (describedBy) attributes["aria-describedby"] = describedBy;
  return EditorView.contentAttributes.of(attributes);
}

export function ComposeEditor({ id, labelId, value, onChange, placeholder, invalid, describedBy }: ComposeEditorProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const [compartments] = useState(() => ({ attributes: new Compartment(), editable: new Compartment() }));
  const [formatError, setFormatError] = useState("");
  const disabledRef = useRef(false);
  const formatErrorId = `${id}-format-error`;
  const contentDescribedBy = [describedBy, formatError ? formatErrorId : undefined].filter(Boolean).join(" ") || undefined;

  useEffect(() => {
    onChangeRef.current = onChange;
  }, [onChange]);

  function format(view: EditorView) {
    const result = formatCompose(view.state.doc.toString());
    if (result.error !== undefined) {
      setFormatError(`Could not format: ${result.error}.`);
      return true;
    }
    setFormatError("");
    if (result.text !== view.state.doc.toString()) view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: result.text } });
    return true;
  }
  const formatRef = useRef(format);
  useEffect(() => {
    formatRef.current = format;
  });

  // The document and ARIA attributes are pushed into the editor by the effects below, so it is not
  // recreated (and its undo history lost) on every keystroke.
  useEffect(() => {
    const view = new EditorView({
      parent: containerRef.current!,
      state: EditorState.create({
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightSpecialChars(),
          history(),
          drawSelection(),
          indentOnInput(),
          bracketMatching(),
          highlightActiveLine(),
          EditorState.tabSize.of(2),
          indentUnit.of("  "),
          keymap.of([{ key: "Shift-Alt-f", run: (target) => formatRef.current(target) }, indentWithTab, ...defaultKeymap, ...historyKeymap]),
          yaml(),
          syntaxHighlighting(highlighting),
          theme,
          placeholderText(placeholder),
          compartments.attributes.of([]),
          compartments.editable.of([]),
          EditorView.updateListener.of((update) => {
            if (!update.docChanged || update.transactions.some((transaction) => transaction.annotation(external))) return;
            setFormatError("");
            onChangeRef.current(update.state.doc.toString());
          }),
        ],
      }),
    });
    viewRef.current = view;
    disabledRef.current = false;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
  }, [compartments, placeholder]);

  useEffect(() => {
    const view = viewRef.current;
    if (view && view.state.doc.toString() !== value) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value }, annotations: [external.of(true), Transaction.addToHistory.of(false)] });
    }
  }, [placeholder, value]);

  useEffect(() => {
    viewRef.current?.dispatch({ effects: compartments.attributes.reconfigure(contentAttributes(id, labelId, invalid, contentDescribedBy)) });
  }, [compartments, placeholder, id, labelId, invalid, contentDescribedBy]);

  // Forms disable their fields with a disabled <fieldset> while saving, which a contenteditable ignores.
  useEffect(() => {
    const isDisabled = containerRef.current?.closest("fieldset:disabled") != null;
    if (isDisabled === disabledRef.current) return;
    disabledRef.current = isDisabled;
    viewRef.current?.dispatch({ effects: compartments.editable.reconfigure(EditorView.editable.of(!isDisabled)) });
  });

  return (
    <div className="space-y-1.5">
      <div
        data-invalid={invalid || undefined}
        className="flex h-80 min-h-40 resize-y flex-col overflow-hidden rounded-md border border-line-input bg-panel hover:border-fg-subtle focus-within:border-info focus-within:ring-1 focus-within:ring-info data-invalid:border-bad"
      >
        <div className="flex h-8 shrink-0 items-center justify-between border-b border-line pr-1 pl-2.5">
          <span className="font-mono text-[11px] text-fg-muted">YAML</span>
          <Button size="sm" variant="ghost" title="Format (Shift+Alt+F)" disabled={value.trim() === ""} onClick={() => {
              const view = viewRef.current;
              if (!view) return;
              format(view);
              view.focus();
            }}>
            <TextAlignStart aria-hidden className="size-3.5" />
            Format
          </Button>
        </div>
        <div ref={containerRef} className="min-h-0 flex-1" />
      </div>
      {formatError && (
        <p id={formatErrorId} role="alert" className="text-xs text-bad">
          {formatError}
        </p>
      )}
    </div>
  );
}
