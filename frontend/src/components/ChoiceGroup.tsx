export interface Choice<T extends string> {
  value: T;
  label: string;
  description: string;
}

export function ChoiceGroup<T extends string>({ legend, name, value, choices, onChange }: { legend: string; name: string; value: T; choices: Choice<T>[]; onChange: (value: T) => void }) {
  return (
    <fieldset>
      <legend className="mb-1.5 text-[13px] font-medium text-fg">{legend}</legend>
      <div className="divide-y divide-line overflow-hidden rounded-md border border-line-strong sm:flex sm:divide-x sm:divide-y-0">
        {choices.map((choice) => (
          <label
            key={choice.value}
            className="flex flex-1 cursor-pointer items-start gap-2.5 px-3 py-2.5 hover:bg-panel has-[:checked]:bg-raised has-[:focus-visible]:outline-2 has-[:focus-visible]:-outline-offset-2 has-[:focus-visible]:outline-info"
          >
            <input
              type="radio"
              name={name}
              value={choice.value}
              checked={value === choice.value}
              onChange={() => onChange(choice.value)}
              className="mt-0.5 accent-fg focus-visible:outline-none"
            />
            <span className="text-[13px] font-medium text-fg">
              {choice.label}
              <span className="mt-0.5 block text-xs font-normal text-fg-muted">{choice.description}</span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}
