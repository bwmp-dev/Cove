import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { notesByteLimit, notesQuery, saveNotes, type Notes as NotesData, type NotesScope } from "../api/notes";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { Button, LoadingRows, Notice, QueryError, Section } from "./ui";

const notesProseClass =
  "min-w-0 space-y-3 text-[13px] leading-relaxed break-words text-fg [&_a]:underline [&_a]:decoration-fg-subtle [&_a]:underline-offset-4 [&_blockquote]:border-l-2 [&_blockquote]:border-line-strong [&_blockquote]:pl-3 [&_blockquote]:text-fg-muted [&_code]:font-mono [&_code]:text-xs [&_h1]:text-sm [&_h1]:font-semibold [&_h2]:text-sm [&_h2]:font-semibold [&_h3]:text-[13px] [&_h3]:font-semibold [&_h4]:font-semibold [&_hr]:border-line [&_li]:mt-1 [&_ol]:list-decimal [&_ol]:pl-5 [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:border [&_pre]:border-line [&_pre]:bg-panel [&_pre]:p-3 [&_ul]:list-disc [&_ul]:pl-5";

const encoder = new TextEncoder();

interface NotesProps {
  scope: NotesScope;
  id: string;
  canEdit: boolean;
}

export function Notes({ scope, id, canEdit }: NotesProps) {
  const notes = useQuery(notesQuery(scope, id));
  const [editing, setEditing] = useState(false);
  const subject = scope === "project" ? "project" : "resource";

  return (
    <Section
      title="Notes"
      description={`Markdown notes for everyone who can read this ${subject}. Stored encrypted. Do not paste passwords or keys; use secret variables for those.`}
      actions={
        canEdit &&
        notes.data &&
        !editing && (
          <Button onClick={() => setEditing(true)}>
            <Pencil aria-hidden className="size-3.5" />
            {notes.data.markdown ? "Edit notes" : "Add notes"}
          </Button>
        )
      }
    >
      {notes.isPending ? (
        <LoadingRows label="Loading notes" rows={2} />
      ) : notes.isError ? (
        <QueryError what="notes" error={notes.error} onRetry={() => void notes.refetch()} retrying={notes.isFetching} />
      ) : editing ? (
        <NotesEditor scope={scope} id={id} saved={notes.data} onDone={() => setEditing(false)} />
      ) : (
        <NotesView notes={notes.data} canEdit={canEdit} />
      )}
    </Section>
  );
}

function NotesView({ notes, canEdit }: { notes: NotesData; canEdit: boolean }) {
  if (!notes.markdown) {
    return <p className="text-[13px] text-fg-muted">{canEdit ? "No notes yet. Add runbooks, contacts or context for the team." : "No notes yet."}</p>;
  }
  return (
    <div className="space-y-3">
      {/* Only the server's sanitized rendering is inserted as HTML; the Markdown source is never interpreted here. */}
      <div className={notesProseClass} dangerouslySetInnerHTML={{ __html: notes.html }} />
      {notes.updated_at && (
        <p className="text-xs text-fg-muted">
          Updated <RelativeTime value={notes.updated_at} />
        </p>
      )}
    </div>
  );
}

function NotesEditor({ scope, id, saved, onDone }: { scope: NotesScope; id: string; saved: NotesData; onDone: () => void }) {
  const fieldId = useId();
  const client = useQueryClient();
  const online = useOnline();
  const [draft, setDraft] = useState(saved.markdown);
  // The version this draft started from. A stale version makes the server refuse the save instead of overwriting.
  const [baseVersion, setBaseVersion] = useState(saved.version);
  const bytes = encoder.encode(draft).length;
  const tooLong = bytes > notesByteLimit;
  const dirty = draft !== saved.markdown || baseVersion !== saved.version;

  // The draft is read from component state rather than passed as a variable, so the mutation cache never holds it.
  const save = useMutation({
    mutationFn: (version: string) => saveNotes(scope, id, draft, version),
    gcTime: 0,
    onSuccess: (data) => {
      client.setQueryData(notesQuery(scope, id).queryKey, data);
      setDraft("");
      save.reset();
      onDone();
    },
  });
  const stale = isStatus(save.error, 409);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!tooLong) save.mutate(baseVersion);
  }

  function cancel() {
    setDraft("");
    save.reset();
    onDone();
  }

  async function reloadLatest(keepDraft: boolean) {
    const latest = await client.fetchQuery({ ...notesQuery(scope, id), staleTime: 0 });
    save.reset();
    setBaseVersion(latest.version);
    if (!keepDraft) setDraft(latest.markdown);
  }

  return (
    <form className="space-y-3" onSubmit={submit}>
      <UnsavedChangesGuard dirty={dirty && !save.isPending} />
      <label htmlFor={fieldId} className="block text-[13px] font-medium text-fg">
        Markdown
      </label>
      <textarea
        id={fieldId}
        className={`${inputClass} min-h-56 resize-y font-mono text-xs leading-relaxed`}
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
        aria-describedby={`${fieldId}-hint`}
        aria-invalid={tooLong || undefined}
        spellCheck
        disabled={save.isPending}
      />
      <p id={`${fieldId}-hint`} className={`text-xs ${tooLong ? "text-bad" : "text-fg-muted"}`}>
        CommonMark headings, lists, links and code. HTML and images are not shown. {bytes.toLocaleString("en")} of {notesByteLimit.toLocaleString("en")} bytes
        {tooLong ? ", shorten the notes to save." : "."}
      </p>
      {stale ? (
        <Notice
          tone="warn"
          title="These notes changed while you were editing"
          action={
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="ghost" onClick={() => void reloadLatest(false)}>
                Discard my draft
              </Button>
              <Button size="sm" onClick={() => void reloadLatest(true)}>
                Keep my draft
              </Button>
            </div>
          }
        >
          Someone saved a newer version. Discard your draft to load it, or keep your draft and save again to replace it.
        </Notice>
      ) : (
        save.isError && (
          <Notice tone="error" title="Could not save notes">
            {errorMessage(save.error)}
          </Notice>
        )
      )}
      <div className="flex flex-wrap items-center justify-end gap-2">
        <Button variant="ghost" onClick={cancel} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" pending={save.isPending} disabled={!online || tooLong || stale || !dirty}>
          Save notes
        </Button>
      </div>
    </form>
  );
}
