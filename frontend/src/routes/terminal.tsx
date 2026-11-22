import { SquareTerminal } from "lucide-react";
import { Select } from "../components/Select";
import { useQuery } from "@tanstack/react-query";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import { request, errorMessage } from "../api/client";
import { nodesQuery } from "../api/queries";
import { Button, EmptyState, Field, LoadingRows, Notice, PageHeader, QueryError, Section } from "../components/ui";
import { useSession } from "../lib/session";
import { inputClass } from "../lib/styles";

interface Output { id: string; data: string | null; offset: number; closed: boolean }

function terminalDocument(): Document {
  const nonce = document.querySelector<HTMLMetaElement>('meta[name="cove-style-nonce"]')?.content;
  // xterm creates dynamic style sheets. Give only those sheets the document's CSP nonce.
  return new Proxy(document, { get(target, property) {
    if (property === "createElement") return (tag: string, options?: ElementCreationOptions) => {
      const element = target.createElement(tag, options);
      if (nonce) {
        if (element instanceof HTMLStyleElement) element.nonce = nonce;
        // xterm's viewport creates its sheet through the global document, bypassing documentOverride.
        const append = element.appendChild.bind(element);
        element.appendChild = <T extends Node>(child: T): T => {
          if (child instanceof HTMLStyleElement) child.nonce = nonce;
          return append(child);
        };
      }
      return element;
    };
    const value: unknown = Reflect.get(target, property, target);
    if (typeof value === "function") return (...args: unknown[]): unknown => Reflect.apply(value, target, args);
    return value;
  } });
}

function NodeTerminal({ id, onDisconnect }: { id: string; onDisconnect: () => void }) {
  const element = useRef<HTMLDivElement>(null);
  const [error, setError] = useState<string | null>(null);
  const [ended, setEnded] = useState(false);
  useEffect(() => {
    if (!element.current) return;
    const term = new Terminal({ documentOverride: terminalDocument(), disableStdin: true, cursorBlink: true, scrollback: 3000, fontSize: 13, theme: { background: "#101012", foreground: "#e7e7e9" } });
    const fit = new FitAddon(); term.loadAddon(fit); term.open(element.current); fit.fit(); term.focus();
    let disposed = false;
    let closed = false;
    let offset = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let writes = Promise.resolve();
    const send = (body: { data?: string; columns?: number; rows?: number }) => {
      writes = writes.then(async () => {
        if (disposed || closed) return;
        await request("POST", `/api/terminal/${id}/input`, body);
      }).catch((reason: unknown) => { closed = true; if (!disposed) setError(errorMessage(reason)); });
    };
    const input = term.onData((data) => {
      const bytes = new TextEncoder().encode(data);
      for (let start = 0; start < bytes.length; start += 8192) send({ data: btoa(String.fromCharCode(...bytes.slice(start, start + 8192))) });
    });
    const resize = new ResizeObserver(() => { fit.fit(); send({ columns: term.cols, rows: term.rows }); });
    resize.observe(element.current);
    async function poll() {
      try {
        const output = await request<Output>("GET", `/api/terminal/${id}?offset=${offset}`);
        if (disposed) return;
        if (output.data) {
          const bytes = Uint8Array.from(atob(output.data), (character) => character.charCodeAt(0));
          await new Promise<void>((resolve) => term.write(bytes, resolve));
          term.options.disableStdin = false;
        }
        offset = output.offset;
        if (output.closed) { closed = true; setEnded(true); return; }
        timer = setTimeout(() => void poll(), 300);
      } catch (reason) { closed = true; if (!disposed) setError(errorMessage(reason)); }
    }
    void poll();
    return () => {
      disposed = true; clearTimeout(timer); input.dispose(); resize.disconnect(); term.dispose();
      void request("DELETE", `/api/terminal/${id}`).catch(() => console.warn("Terminal disconnect could not be confirmed; the worker expires abandoned sessions."));
    };
  }, [id]);
  return <Section title="Node shell" actions={<Button onClick={onDisconnect}>Disconnect</Button>}>
    {error && <Notice tone="error" title="Terminal disconnected">{error}</Notice>}
    {ended && <Notice title="Shell exited">Disconnect to open a new session.</Notice>}
    <div ref={element} className="mt-3 h-96 min-w-0 overflow-hidden" aria-label="Interactive node terminal" />
  </Section>;
}

function TerminalControls() {
  const nodes = useQuery(nodesQuery);
  const [node, setNode] = useState("");
  const [session, setSession] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const mounted = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  async function connect() {
    setPending(true); setError(null);
    try { const result = await request<Output>("POST", "/api/terminal", { node_id: node, columns: 100, rows: 24 }); if (mounted.current) setSession(result.id); else await request("DELETE", `/api/terminal/${result.id}`); }
    catch (reason) { setError(errorMessage(reason)); }
    finally { setPending(false); }
  }
  if (nodes.isPending) return <LoadingRows label="Loading nodes" />;
  if (nodes.isError) return <Section title="Nodes"><QueryError what="nodes" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} /></Section>;
  return <>
    <Section title="Connect to a node" description="The shell runs as the worker account, including its Docker access. Sessions close after five minutes disconnected or one hour total. Commands and output are not recorded.">
      <div className="flex flex-wrap items-end gap-3">
        <Field id="terminal-node" label="Node"><Select id="terminal-node" className={inputClass} value={node} disabled={session !== null || pending} onChange={(event) => setNode(event.target.value)}><option value="">Select a node</option>{nodes.data.filter((item) => item.state === "ready" && item.capabilities?.terminal).map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</Select></Field>
        <Button variant="primary" disabled={!node || session !== null} pending={pending} onClick={() => void connect()}>Connect</Button>
      </div>
      {!nodes.data.some((item) => item.capabilities?.terminal) && <p className="mt-3 text-sm text-fg-muted">Upgrade and verify a worker to enable terminal access.</p>}
      {error && <Notice tone="error" title="Could not connect">{error}</Notice>}
    </Section>
    {session && <NodeTerminal key={session} id={session} onDisconnect={() => setSession(null)} />}
  </>;
}

export function TerminalPage() {
  const session = useSession();
  return <><PageHeader icon={<SquareTerminal aria-hidden />} title="Terminal" description="Interactive access to your nodes over Cove's authenticated SSH connection." />{session?.is_admin ? <TerminalControls /> : <EmptyState title="Instance administrators only">Node shell access can affect all teams sharing a worker.</EmptyState>}</>;
}
