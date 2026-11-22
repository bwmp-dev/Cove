import { reasonError } from "../lib/operations";
import { inputClass } from "../lib/styles";
import { Field } from "./ui";

interface ReasonFieldProps {
  id: string;
  value: string;
  onChange: (value: string) => void;
  label?: string;
  hint?: string;
  required?: boolean;
  disabled?: boolean;
}

// A short free-text reason recorded with the operation and in the audit log. It is visible to everyone who can
// read the operation, so the hint warns against secrets.
export function ReasonField({ id, value, onChange, label = "Reason", hint = "Recorded with the operation and in the audit log. Visible to anyone who can read this resource; do not include secrets.", required = false, disabled }: ReasonFieldProps) {
  const error = value ? reasonError(value, required) : undefined;
  return (
    <Field id={id} label={label} optional={!required} hint={hint} error={error}>
      <textarea
        id={id}
        className={inputClass}
        rows={2}
        value={value}
        required={required}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-error` : `${id}-hint`}
        onChange={(event) => onChange(event.target.value)}
      />
    </Field>
  );
}
