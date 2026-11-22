import { Check, ChevronDown } from "lucide-react";
import { Children, Fragment, isValidElement, useCallback, useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode, type SelectHTMLAttributes } from "react";
import { createPortal } from "react-dom";
import { inputClass } from "../lib/styles";
import { floatingPanelClass, useFloatingPanel } from "./useFloatingPanel";

type SelectProps = Omit<SelectHTMLAttributes<HTMLSelectElement>, "multiple" | "size" | "value" | "defaultValue" | "autoFocus"> & { value?: string | number; defaultValue?: string | number };
interface Option { value: string; label: string; disabled: boolean; group?: string }

function optionText(children: ReactNode): string {
  return Children.toArray(children).map(child => typeof child === "string" || typeof child === "number" ? String(child) : "").join("");
}

function optionsFrom(children: ReactNode, group?: string, disabled = false): Option[] {
  const options: Option[] = [];
  Children.forEach(children, child => {
    if (!isValidElement<{ value?: string | number; label?: string; disabled?: boolean; children?: ReactNode }>(child)) return;
    if (child.type === "option") {
      const label = child.props.label ?? optionText(child.props.children);
      options.push({ value: String(child.props.value ?? optionText(child.props.children)), label, disabled: disabled || !!child.props.disabled, group });
    } else if (child.type === "optgroup" || child.type === Fragment) {
      options.push(...optionsFrom(child.props.children, child.type === "optgroup" ? child.props.label : group, disabled || !!child.props.disabled));
    }
  });
  return options;
}

export function Select({ id: providedID, className = inputClass, children, value, defaultValue, disabled, required, onChange, onInvalid, onBlur, ...props }: SelectProps) {
  const generatedID = useId();
  const id = providedID ?? generatedID;
  const listID = `${generatedID}-options`;
  const button = useRef<HTMLButtonElement>(null);
  const native = useRef<HTMLSelectElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [portalHost, setPortalHost] = useState<HTMLElement | null>(null);
  const [internalValue, setInternalValue] = useState(String(defaultValue ?? ""));
  const [active, setActive] = useState(0);
  const [invalid, setInvalid] = useState(false);
  const typed = useRef({ text: "", at: 0 });
  const options = optionsFrom(children);
  const selectedValue = String(value ?? internalValue);
  const selectedIndex = options.findIndex(option => option.value === selectedValue);
  const selected = options[selectedIndex] ?? options.find(option => !option.disabled) ?? options[0];
  const close = useCallback(() => setOpen(false), []);
  useFloatingPanel({ open, id: listID, trigger: button, panel, onClose: close });

  useEffect(() => {
    const form = native.current?.form;
    function reset() { setInternalValue(String(defaultValue ?? "")); setInvalid(false); setOpen(false); }
    form?.addEventListener("reset", reset);
    return () => form?.removeEventListener("reset", reset);
  }, [defaultValue]);

  useEffect(() => {
    if (open) document.getElementById(`${listID}-${active}`)?.scrollIntoView?.({ block: "nearest" });
  }, [active, open, listID]);

  function show() {
    if (disabled || native.current?.matches(":disabled")) return;
    setPortalHost(button.current?.closest("dialog") ?? document.body);
    setActive(selectedIndex >= 0 && !options[selectedIndex]?.disabled ? selectedIndex : Math.max(0, options.findIndex(option => !option.disabled)));
    setOpen(true);
  }

  function choose(index: number) {
    const option = options[index];
    const element = native.current;
    if (!option || option.disabled || !element || element.matches(":disabled")) return;
    element.value = option.value;
    element.dispatchEvent(new Event("change", { bubbles: true }));
    setInvalid(false);
    setOpen(false);
    button.current?.focus();
  }

  function keyDown(event: KeyboardEvent<HTMLButtonElement>) {
    if (event.key === "Escape") { if (open) { event.preventDefault(); event.stopPropagation(); } setOpen(false); return; }
    if (event.key === "Tab") { setOpen(false); return; }
    if (event.key === "Enter" || event.key === " ") { event.preventDefault(); if (open) choose(active); else show(); return; }
    const enabled = options.flatMap((option, index) => option.disabled ? [] : [index]);
    if (!enabled.length) return;
    if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
      event.preventDefault();
      if (!open) { show(); return; }
      const current = enabled.indexOf(active);
      const index = event.key === "Home" ? 0 : event.key === "End" ? enabled.length - 1 : Math.max(0, Math.min(enabled.length - 1, current + (event.key === "ArrowDown" ? 1 : -1)));
      setActive(enabled[index] ?? 0);
    } else if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
      event.preventDefault();
      const now = Date.now();
      const text = (now - typed.current.at < 700 ? typed.current.text : "") + event.key.toLocaleLowerCase();
      typed.current = { text, at: now };
      const search = [...enabled.filter(index => index > (open ? active : selectedIndex)), ...enabled.filter(index => index <= (open ? active : selectedIndex))];
      const index = search.find(index => options[index]?.label.toLocaleLowerCase().startsWith(text)) ?? search.find(index => options[index]?.label.toLocaleLowerCase().startsWith(event.key.toLocaleLowerCase()));
      if (index !== undefined) { if (open) setActive(index); else choose(index); }
    }
  }

  return <>
    <select {...props} ref={native} id={`${id}-native`} tabIndex={-1} aria-hidden="true" aria-label={undefined} aria-labelledby={undefined} aria-invalid={undefined} className="sr-only" disabled={disabled} required={required} value={value} defaultValue={defaultValue}
      onChange={event => { setInternalValue(event.target.value); setInvalid(false); onChange?.(event); }}
      onBlur={onBlur}
      onFocus={() => button.current?.focus()}
      onInvalid={event => { onInvalid?.(event); event.preventDefault(); setInvalid(true); button.current?.focus(); }}
    >{children}</select>
    <button ref={button} id={id} type="button" role="combobox" aria-haspopup="listbox" aria-expanded={open} aria-controls={open ? listID : undefined} aria-activedescendant={open && options[active] ? `${listID}-${active}` : undefined}
      aria-label={props["aria-label"]} aria-labelledby={props["aria-labelledby"]} aria-describedby={invalid ? `${id}-validation` : props["aria-describedby"]} aria-invalid={invalid || props["aria-invalid"]} aria-required={required}
      disabled={disabled} title={props.title ?? selected?.label} data-value={selected?.value ?? ""} data-native-select={`${id}-native`} onClick={() => open ? close() : show()} onKeyDown={keyDown}
      onBlur={event => { if (!panel.current?.contains(event.relatedTarget)) close(); }}
      className={`${className} group flex min-h-9 items-center justify-between gap-3 text-left transition-colors aria-expanded:border-info aria-expanded:ring-1 aria-expanded:ring-info disabled:cursor-not-allowed disabled:bg-surface pointer-coarse:min-h-11`}
    ><span className="min-w-0 flex-1 truncate">{selected?.label ?? "Select an option"}</span><ChevronDown aria-hidden className="size-3.5 shrink-0 text-fg-muted transition-transform group-aria-expanded:rotate-180" /></button>
    {invalid && <p id={`${id}-validation`} className="mt-1 text-xs text-bad">Choose an option.</p>}
    {open && createPortal(<div ref={panel} id={listID} role="listbox" tabIndex={-1} aria-label={props["aria-label"] ?? "Options"} aria-labelledby={props["aria-labelledby"] ?? id} className={floatingPanelClass} onMouseDown={event => event.preventDefault()}>
      <SelectOptions options={options} listID={listID} active={active} selectedValue={selected?.value} onActive={setActive} onChoose={choose} />
    </div>, portalHost ?? document.body)}
  </>;
}

function SelectOptions({ options, listID, active, selectedValue, onActive, onChoose }: {
  options: Option[];
  listID: string;
  active: number;
  selectedValue?: string;
  onActive: (index: number) => void;
  onChoose: (index: number) => void;
}) {
  return <>
      {options.map((option, index) => <Fragment key={option.value}>
        {option.group && option.group !== options[index - 1]?.group && <div role="presentation" className="px-2.5 pb-1 pt-2 text-xs font-medium text-fg-muted">{option.group}</div>}
        <div id={`${listID}-${index}`} role="option" tabIndex={-1} onKeyDown={event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onChoose(index); } }} aria-selected={selectedValue === option.value} aria-disabled={option.disabled || undefined} data-value={option.value} onPointerMove={() => { if (!option.disabled) onActive(index); }} onClick={() => onChoose(index)}
          className={`flex min-h-9 cursor-default items-center gap-2 rounded-md px-2.5 py-2 text-[13px] pointer-coarse:min-h-11 ${option.disabled ? "text-fg-disabled" : index === active ? "bg-hover text-fg" : "text-fg-muted"} ${selectedValue === option.value ? "font-medium text-fg" : ""}`}
        ><span className="min-w-0 flex-1 break-words">{option.label}</span>{selectedValue === option.value && <Check aria-hidden className="size-4 shrink-0 text-info" />}</div>
      </Fragment>)}
      {!options.length && <p className="px-2.5 py-2 text-sm text-fg-muted">No options available</p>}
  </>;
}
