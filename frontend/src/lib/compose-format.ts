import { parseDocument, Scalar, visit } from "yaml";

export type FormatResult = { text: string; error?: undefined } | { text?: undefined; error: string };

// Compose files are read by parsers with YAML 1.1 and 1.2 rules, so a quote is only dropped when
// both read the plain text back as the same string ("22:22" is a number and "on" a boolean in 1.1).
// Port and address mappings starting with a digit stay quoted, as the Compose documentation advises.
function plainIsSafe(value: string) {
  if (value === "" || /[\n\r\t]/.test(value) || /^\d.*:/.test(value)) return false;
  return (["1.1", "1.2"] as const).every((version) => {
    const document = parseDocument(value, { version, logLevel: "silent" });
    return document.errors.length === 0 && document.toJS() === value;
  });
}

// Rewrites flow ({...}, [...]) collections as indented block YAML and removes unnecessary quotes.
export function formatCompose(source: string): FormatResult {
  const document = parseDocument(source);
  const [error] = document.errors;
  if (error) return { error: error.message.replace(/:?\n[\s\S]*/, "") };
  visit(document, {
    Collection(_, node) {
      node.flow = false;
    },
    Scalar(_, node) {
      const quoted = node.type === Scalar.QUOTE_DOUBLE || node.type === Scalar.QUOTE_SINGLE;
      if (quoted && typeof node.value === "string" && plainIsSafe(node.value)) node.type = Scalar.PLAIN;
    },
  });
  return { text: document.toString({ indent: 2, lineWidth: 0 }) };
}
