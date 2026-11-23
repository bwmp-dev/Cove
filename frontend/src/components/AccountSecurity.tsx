import { useQuery } from "@tanstack/react-query";
import { KeyRound, Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import {
  accountSignInMethodsQuery,
  identitiesQuery,
  passkeysQuery,
  signInMethodsQuery,
  useBeginTotp,
  useConfirmTotp,
  useDeletePasskey,
  useOidcStart,
  useRegenerateRecoveryCodes,
  useRegisterPasskey,
  useRemoveTotp,
  useRenamePasskey,
  useUnlinkIdentity,
  type AccountSignInMethods,
  type LinkedIdentity,
  type Passkey,
} from "../api/security";
import { publicLdapProvidersQuery, useLinkLdapIdentity, type PublicLdapProvider } from "../api/directory";
import { ldapProviderId, ldapSignInErrorMessage } from "../lib/directory";
import { useOnline } from "../lib/hooks";
import { reauthErrorMessage, reauthLabels, reauthMethods, reauthProof, type ReauthFactors, type ReauthMethod, type ReauthProof } from "../lib/reauth";
import { useSession } from "../lib/session";
import type { SignInProvider } from "../lib/sign-in";
import { choiceClass, describedBy, inputClass } from "../lib/styles";
import { groupSecret, parseProvisioningURI } from "../lib/totp";
import { PasskeyCancelled, passkeysSupported } from "../lib/webauthn";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { QrCode } from "./QrCode";
import { RecoveryCodes } from "./RecoveryCodes";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

function CurrentPasswordField({ id, value, onChange, error }: { id: string; value: string; onChange: (value: string) => void; error?: string }) {
  return (
    <Field id={id} label="Current password" error={error}>
      <input
        id={id}
        className={inputClass}
        type="password"
        autoComplete="current-password"
        required
        maxLength={72}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, error)}
      />
    </Field>
  );
}

function TotpSetup({ onFinished }: { onFinished: () => void }) {
  const id = useId();
  const session = useSession();
  const online = useOnline();
  const begin = useBeginTotp();
  const confirm = useConfirmTotp();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [codeError, setCodeError] = useState<string>();
  const needsPassword = session?.has_password ?? true;

  if (confirm.data) {
    return (
      <RecoveryCodes
        codes={confirm.data.recovery_codes}
        account={session?.email || session?.name || "your account"}
        onDone={() => {
          confirm.reset();
          begin.reset();
          onFinished();
        }}
      />
    );
  }

  const provisioning = begin.data ? parseProvisioningURI(begin.data.provisioning_uri) : null;
  if (begin.data) {
    const expired = isStatus(confirm.error, 400) && errorMessage(confirm.error).toLowerCase().includes("expired");
    return (
      <form
        className="space-y-4"
        noValidate
        onSubmit={(event: FormEvent<HTMLFormElement>) => {
          event.preventDefault();
          const value = code.replace(/\s/g, "");
          const problem = /^\d{6}$/.test(value) ? undefined : "Enter the 6-digit code shown in your authenticator app.";
          setCodeError(problem);
          if (!problem) confirm.mutate(value, { onError: () => setCode("") });
        }}
      >
        <ol className="list-decimal space-y-1 pl-5 text-[13px] text-fg-muted">
          <li>Scan the QR code with an authenticator app, or enter the key manually.</li>
          <li>Enter the 6-digit code the app shows to confirm. The setup expires after 10 minutes.</li>
        </ol>
        <div className="flex flex-wrap items-start gap-4">
          <QrCode value={begin.data.provisioning_uri} label="QR code for your authenticator app" />
          {provisioning && (
            <div className="min-w-0 flex-1 basis-56 space-y-2">
              <p className="text-xs text-fg-muted">Manual entry key (time-based, 6 digits, 30 seconds)</p>
              <p className="font-mono text-[13px] break-all text-fg">{groupSecret(provisioning.secret)}</p>
              <CopyButton text={provisioning.secret} label="Copy key" />
            </div>
          )}
        </div>
        <Field id={`${id}-code`} label="Verification code" error={codeError}>
          <input
            id={`${id}-code`}
            className={`${inputClass} font-mono sm:max-w-40`}
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={7}
            value={code}
            onChange={(event) => setCode(event.target.value)}
            aria-invalid={codeError ? true : undefined}
            aria-describedby={describedBy(`${id}-code`, codeError)}
          />
        </Field>
        {confirm.isError && (
          <Notice tone="error" title="Could not confirm the authenticator app">
            {expired ? "The setup expired. Start again to get a new QR code." : errorMessage(confirm.error)}
          </Notice>
        )}
        <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-4">
          <Button variant="ghost" onClick={() => { begin.reset(); confirm.reset(); onFinished(); }} disabled={confirm.isPending}>
            Cancel
          </Button>
          {expired ? (
            <Button variant="primary" onClick={() => { confirm.reset(); begin.reset(); }}>
              Start again
            </Button>
          ) : (
            <Button type="submit" variant="primary" disabled={!online} pending={confirm.isPending}>
              Confirm and show recovery codes
            </Button>
          )}
        </div>
      </form>
    );
  }

  return (
    <form
      className="space-y-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (needsPassword && password === "") return;
        begin.mutate(needsPassword ? password : "", { onSettled: () => setPassword("") });
      }}
    >
      {needsPassword && <CurrentPasswordField id={`${id}-password`} value={password} onChange={setPassword} />}
      {begin.isError && <Notice tone="error" title="Could not start setup">{errorMessage(begin.error)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onFinished} disabled={begin.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online || (needsPassword && password === "")} pending={begin.isPending}>
          Show QR code
        </Button>
      </div>
    </form>
  );
}

export function useReauth(factors: ReauthFactors) {
  const session = useSession();
  const methods = reauthMethods(session?.has_password ?? true, factors);
  const [chosen, setChosen] = useState<ReauthMethod>();
  const [value, setValue] = useState("");
  const [error, setError] = useState<string>();
  const method = chosen && methods.includes(chosen) ? chosen : (methods[0] ?? "recent");
  return {
    methods,
    method,
    value,
    error,
    setValue,
    setMethod: (next: ReauthMethod) => {
      setChosen(next);
      setValue("");
      setError(undefined);
    },
    // Returns the request body, or null after showing why the entry is incomplete.
    proof: (): ReauthProof | null => {
      const result = reauthProof(method, value);
      if ("error" in result) {
        setError(result.error);
        return null;
      }
      setError(undefined);
      return result.proof;
    },
    clear: () => setValue(""),
  };
}

export type Reauth = ReturnType<typeof useReauth>;

export function ReauthFields({ id, reauth }: { id: string; reauth: Reauth }) {
  const recent = (
    <p className="text-[13px] text-fg-muted">
      Works within ten minutes of signing in with a passkey or finishing two-step verification in this browser. There is nothing to enter.
    </p>
  );
  return (
    <div className="space-y-3">
      {reauth.methods.length > 1 ? (
        <fieldset>
          <legend className="mb-1.5 text-[13px] font-medium text-fg">Confirm it's you with</legend>
          <div className="grid gap-2 sm:grid-cols-3">
            {reauth.methods.map((item) => (
              <label key={item} className={choiceClass}>
                <input type="radio" name={`${id}-method`} value={item} checked={reauth.method === item} onChange={() => reauth.setMethod(item)} className="accent-info" />
                {reauthLabels[item]}
              </label>
            ))}
          </div>
        </fieldset>
      ) : (
        <p className="text-[13px] font-medium text-fg">Confirm it's you</p>
      )}
      {reauth.method === "password" && <CurrentPasswordField id={`${id}-password`} value={reauth.value} onChange={reauth.setValue} error={reauth.error} />}
      {reauth.method === "code" && (
        <Field id={`${id}-code`} label="Code" hint="The current 6-digit code from your authenticator app, or an unused recovery code, which is then used up." error={reauth.error}>
          <input
            id={`${id}-code`}
            className={`${inputClass} font-mono sm:max-w-64`}
            autoComplete="one-time-code"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={64}
            value={reauth.value}
            onChange={(event) => reauth.setValue(event.target.value)}
            aria-invalid={reauth.error ? true : undefined}
            aria-describedby={describedBy(`${id}-code`, reauth.error, true)}
          />
        </Field>
      )}
      {reauth.method === "recent" && recent}
    </div>
  );
}

const totpPolicyRefusal = "This instance or a team you belong to requires a second factor, and you have no passkey. Add a passkey first, then remove or replace the authenticator app.";

function RemoveTotp({ factors, replacing, onRemoved, onCancel }: { factors: AccountSignInMethods; replacing: boolean; onRemoved: () => void; onCancel: () => void }) {
  const id = useId();
  const online = useOnline();
  const remove = useRemoveTotp();
  const reauth = useReauth(factors);
  const refused = isStatus(remove.error, 409);
  return (
    <form
      className="space-y-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const proof = reauth.proof();
        if (proof) remove.mutate(proof, { onSuccess: onRemoved, onSettled: reauth.clear });
      }}
    >
      <p className="text-[13px] text-fg-muted">
        {replacing ? "Cove removes the current authenticator app, then shows a new QR code to scan. " : "Codes from this app stop working. "}
        Keep the old entry in your authenticator until Cove confirms the removal. This signs out your other sessions
        {factors.passkey_enrolled ? "." : " and deletes your recovery codes, because you have no passkey."}
      </p>
      <ReauthFields id={id} reauth={reauth} />
      {remove.isError && (
        <Notice tone="error" title={refused ? "A second factor is required" : "Could not remove the authenticator app"}>
          {refused ? totpPolicyRefusal : reauthErrorMessage(remove.error, reauth.method)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-4">
        <Button variant="ghost" onClick={onCancel} disabled={remove.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant={replacing ? "primary" : "danger"} disabled={!online} pending={remove.isPending}>
          {replacing ? "Remove and set up a new app" : "Remove authenticator app"}
        </Button>
      </div>
    </form>
  );
}

function AuthenticatorApp({ factors }: { factors: AccountSignInMethods }) {
  const [mode, setMode] = useState<"setup" | "remove" | "replace" | null>(null);
  const [removed, setRemoved] = useState(false);
  // Mirrors the server's check for the selected team; other teams can still refuse, which the form explains.
  const blocked = factors.second_factor_required && !factors.passkey_enrolled;

  if (mode === "setup") {
    return (
      <Subsection title="Authenticator app" description="Scan the new QR code to finish. Setting it up signs out your other sessions and issues new recovery codes.">
        <TotpSetup onFinished={() => setMode(null)} />
      </Subsection>
    );
  }

  if (factors.totp_enrolled) {
    return (
      <Subsection
        title="Authenticator app"
        description="Time-based codes from your authenticator app protect sign-in. Replace it when you change devices."
        actions={
          mode === null && (
            <>
              <StatusLabel tone="ok" label="Set up" />
              <Button disabled={blocked} onClick={() => setMode("replace")}>
                Replace
              </Button>
              <Button variant="ghost" disabled={blocked} onClick={() => setMode("remove")}>
                Remove
              </Button>
            </>
          )
        }
      >
        {mode === null && blocked && <p className="text-xs text-fg-muted">{totpPolicyRefusal}</p>}
        {(mode === "remove" || mode === "replace") && (
          <RemoveTotp
            factors={factors}
            replacing={mode === "replace"}
            onCancel={() => setMode(null)}
            onRemoved={() => {
              setRemoved(mode === "remove");
              setMode(mode === "replace" ? "setup" : null);
            }}
          />
        )}
      </Subsection>
    );
  }

  return (
    <Subsection
      title="Authenticator app"
      description="Use a time-based code from an app such as a password manager. Setting it up signs out your other sessions and issues recovery codes."
      actions={
        <Button
          onClick={() => {
            setRemoved(false);
            setMode("setup");
          }}
        >
          Set up authenticator app
        </Button>
      }
    >
      {removed && <Notice tone="success" title="Authenticator app removed">You can now delete its entry from your authenticator.</Notice>}
    </Subsection>
  );
}

function RegenerateRecoveryCodes({ factors, onClose }: { factors: AccountSignInMethods; onClose: () => void }) {
  const id = useId();
  const session = useSession();
  const online = useOnline();
  const regenerate = useRegenerateRecoveryCodes();
  const reauth = useReauth(factors);

  if (regenerate.data) {
    return (
      <RecoveryCodes
        codes={regenerate.data.recovery_codes}
        account={session?.email || session?.name || "your account"}
        onDone={() => {
          regenerate.reset();
          onClose();
        }}
      />
    );
  }

  return (
    <form
      className="space-y-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const proof = reauth.proof();
        if (proof) regenerate.mutate(proof, { onSettled: reauth.clear });
      }}
    >
      <p className="text-[13px] text-fg-muted">Generating a new set of ten codes stops every current recovery code from working.</p>
      <ReauthFields id={id} reauth={reauth} />
      {regenerate.isError && <Notice tone="error" title="Could not generate recovery codes">{reauthErrorMessage(regenerate.error, reauth.method)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2 border-t border-line pt-4">
        <Button variant="ghost" onClick={onClose} disabled={regenerate.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={regenerate.isPending}>
          Generate new codes
        </Button>
      </div>
    </form>
  );
}

function RecoveryCodesSetting({ factors }: { factors: AccountSignInMethods }) {
  const [regenerating, setRegenerating] = useState(false);
  const enrolled = factors.totp_enrolled || factors.passkey_enrolled;
  const remaining = factors.recovery_codes_remaining;
  return (
    <Subsection
      title="Recovery codes"
      description="Each code signs you in once if you lose your authenticator app or passkeys."
      actions={
        enrolled &&
        !regenerating && (
          <>
            <StatusLabel tone={remaining === 0 ? "warn" : "neutral"} label={remaining === 1 ? "1 unused code" : `${remaining} unused codes`} />
            <Button onClick={() => setRegenerating(true)}>Generate new codes</Button>
          </>
        )
      }
    >
      {!enrolled && <p className="text-xs text-fg-muted">Available after you set up an authenticator app or a passkey.</p>}
      {enrolled && !regenerating && remaining === 0 && (
        <p className="text-xs text-fg-muted">No unused codes are left. Generate new codes so you can still sign in if you lose your second factor.</p>
      )}
      {regenerating && <RegenerateRecoveryCodes factors={factors} onClose={() => setRegenerating(false)} />}
    </Subsection>
  );
}

export function TwoStepVerification() {
  const factors = useQuery(accountSignInMethodsQuery);
  return (
    <Section title="Two-step verification" description="A second factor protects your account if your password is exposed." width="form" flush>
      {factors.isPending ? (
        <LoadingRows label="Loading two-step verification" rows={2} />
      ) : factors.isError ? (
        <div className="p-3">
          <QueryError what="two-step verification" error={factors.error} onRetry={() => void factors.refetch()} retrying={factors.isFetching} />
        </div>
      ) : (
        <div className="divide-y divide-line">
          {factors.data.enrolment_required && (
            <div className="p-4">
              <Notice tone="warn" title="Two-step verification is required">
                This instance or the selected team requires a second factor. Add an authenticator app or a passkey to use projects and resources again.
              </Notice>
            </div>
          )}
          <AuthenticatorApp factors={factors.data} />
          <RecoveryCodesSetting factors={factors.data} />
          <Subsection layout="row" title="Policy">
            <div className="flex @lg:justify-end">
              <StatusLabel tone={factors.data.second_factor_required ? "info" : "neutral"} label={factors.data.second_factor_required ? "Required for this account" : "Optional for this account"} />
            </div>
          </Subsection>
        </div>
      )}
    </Section>
  );
}

const nameBytes = (value: string) => new TextEncoder().encode(value.trim()).length;

function PasskeyRow({ passkey, lastFactor }: { passkey: Passkey; lastFactor: boolean }) {
  const id = useId();
  const online = useOnline();
  const rename = useRenamePasskey();
  const remove = useDeletePasskey();
  const [name, setName] = useState<string | null>(null);
  const [nameError, setNameError] = useState<string>();
  const [deleting, setDeleting] = useState(false);
  return (
    <li className="px-4 py-2.5">
      {name === null ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="min-w-0 flex-1">
            <p className="text-[13px] font-medium break-words text-fg">{passkey.name}</p>
            <p className="text-xs text-fg-muted">
              Added <RelativeTime value={passkey.created_at} />
            </p>
          </div>
          <div className="flex gap-2">
            <Button variant="ghost" size="sm" disabled={!online} onClick={() => setName(passkey.name)}>
              Rename<span className="sr-only"> {passkey.name}</span>
            </Button>
            <Button variant="ghost" size="sm" disabled={!online} pending={remove.isPending} onClick={() => setDeleting(true)}>
              Delete<span className="sr-only"> {passkey.name}</span>
            </Button>
          </div>
        </div>
      ) : (
        <form
          className="flex flex-wrap items-start gap-2"
          noValidate
          onSubmit={(event) => {
            event.preventDefault();
            const size = nameBytes(name);
            const problem = size === 0 || size > 120 ? "Use 1 to 120 characters." : undefined;
            setNameError(problem);
            if (!problem) rename.mutate({ id: passkey.id, name: name.trim() }, { onSuccess: () => setName(null) });
          }}
        >
          <div className="min-w-0 flex-1 basis-56 space-y-1">
            <label htmlFor={`${id}-name`} className="block text-xs text-fg-muted">
              Passkey name
            </label>
            <input
              id={`${id}-name`}
              className={inputClass}
              value={name}
              onChange={(event) => setName(event.target.value)}
              aria-invalid={nameError ? true : undefined}
              aria-describedby={nameError ? `${id}-name-error` : undefined}
            />
            {nameError && (
              <p id={`${id}-name-error`} className="text-xs text-bad">
                {nameError}
              </p>
            )}
          </div>
          <div className="flex gap-2 sm:pt-5">
            <Button variant="ghost" onClick={() => { setName(null); setNameError(undefined); rename.reset(); }} disabled={rename.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={!online} pending={rename.isPending}>
              Save name
            </Button>
          </div>
        </form>
      )}
      {rename.isError && <div className="mt-2"><Notice tone="error" title="Could not rename the passkey">{errorMessage(rename.error)}</Notice></div>}
      {remove.isError && <div className="mt-2"><Notice tone="error" title="Could not delete the passkey">{errorMessage(remove.error)}</Notice></div>}
      <ConfirmDialog
        open={deleting}
        title={`Delete ${passkey.name}?`}
        confirmLabel="Delete passkey"
        danger
        onCancel={() => setDeleting(false)}
        onConfirm={() => {
          remove.mutate(passkey.id);
          setDeleting(false);
        }}
      >
        You can no longer sign in with this passkey. Remove it from the authenticator too.
        {lastFactor && " It is your last second factor and a second factor is required, so projects and resources stay blocked until you add another."}
      </ConfirmDialog>
    </li>
  );
}

function AddPasskey({ onClose }: { onClose: () => void }) {
  const id = useId();
  const session = useSession();
  const online = useOnline();
  const register = useRegisterPasskey();
  const [password, setPassword] = useState("");
  const needsPassword = session?.has_password ?? true;
  const cancelled = register.error instanceof PasskeyCancelled;
  return (
    <form
      className="space-y-4 border-t border-line bg-panel p-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (needsPassword && password === "") return;
        register.mutate(needsPassword ? password : "", { onSuccess: onClose, onSettled: () => setPassword("") });
      }}
    >
      <p className="text-[13px] text-fg-muted">Your browser asks you to create a passkey with this device, a security key or your password manager. Adding a passkey signs out your other sessions.</p>
      {needsPassword && <CurrentPasswordField id={`${id}-password`} value={password} onChange={setPassword} />}
      {register.isError &&
        (cancelled ? (
          <p role="status" className="text-xs text-fg-muted">The passkey prompt was closed. Nothing was saved.</p>
        ) : (
          <Notice tone="error" title="Could not add the passkey">
            {register.error instanceof Error && register.error.name === "PasskeyFailed" ? register.error.message : errorMessage(register.error)}
          </Notice>
        ))}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={register.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online || (needsPassword && password === "")} pending={register.isPending}>
          <KeyRound aria-hidden className="size-3.5" />
          Create passkey
        </Button>
      </div>
    </form>
  );
}

export function Passkeys() {
  const passkeys = useQuery(passkeysQuery);
  const factors = useQuery(accountSignInMethodsQuery);
  const online = useOnline();
  const [adding, setAdding] = useState(false);
  const supported = passkeysSupported();
  const lastFactor = Boolean(factors.data?.second_factor_required && !factors.data.totp_enrolled && passkeys.data?.length === 1);
  return (
    <Section
      title="Passkeys"
      description="Sign in without a password, or use a passkey as your second factor. Up to 20 per account."
      width="form"
      flush
      actions={
        supported && !adding && (
          <Button disabled={!online || (passkeys.data?.length ?? 0) >= 20} onClick={() => setAdding(true)}>
            <Plus aria-hidden className="size-3.5" />
            Add passkey
          </Button>
        )
      }
    >
      {!supported && <p className="border-b border-line px-4 py-3 text-[13px] text-fg-muted">This browser does not support passkeys. Existing passkeys can still be renamed or deleted.</p>}
      {passkeys.isPending ? (
        <LoadingRows label="Loading passkeys" rows={2} />
      ) : passkeys.isError ? (
        <div className="p-3">
          <QueryError what="passkeys" error={passkeys.error} onRetry={() => void passkeys.refetch()} retrying={passkeys.isFetching} />
        </div>
      ) : passkeys.data.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">No passkeys yet.</p>
      ) : (
        <ul className="divide-y divide-line">
          {passkeys.data.map((passkey) => (
            <PasskeyRow key={passkey.id} passkey={passkey} lastFactor={lastFactor} />
          ))}
        </ul>
      )}
      {adding && <AddPasskey onClose={() => setAdding(false)} />}
    </Section>
  );
}

const identityRefusal =
  "This is your last way to sign in. Add a passkey or link another identity first. A password only counts while password sign-in is turned on for this instance, and a directory only while it is enabled.";

const provisioningRefusal = "Your organization's SCIM provisioning manages this sign-in identity. Ask an instance administrator to detach provisioning for your account first.";

function unlinkRefusal(error: unknown) {
  return errorMessage(error).toLowerCase().includes("scim") ? provisioningRefusal : identityRefusal;
}

function identityDetail(identity: LinkedIdentity) {
  if (identity.provider_id === "github") return <>Account ID <span className="font-mono">{identity.subject}</span></>;
  // The directory subject is an opaque immutable ID; showing it would only invite people to treat it as a credential.
  if (ldapProviderId(identity.provider_id) !== null) return "Directory account";
  return <>Subject <span className="font-mono">{identity.subject}</span></>;
}

function IdentityRow({ identity, name, factors }: { identity: LinkedIdentity; name: string; factors: AccountSignInMethods }) {
  const id = useId();
  const online = useOnline();
  const unlink = useUnlinkIdentity();
  const reauth = useReauth(factors);
  const [open, setOpen] = useState(false);
  const refused = isStatus(unlink.error, 409);
  return (
    <li className="px-4 py-2.5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-[13px] font-medium break-words text-fg">{name}</p>
          <p className="text-xs break-all text-fg-muted">{identityDetail(identity)}</p>
        </div>
        {!open && (
          <Button variant="ghost" size="sm" disabled={!online} onClick={() => setOpen(true)}>
            Unlink<span className="sr-only"> {name}</span>
          </Button>
        )}
      </div>
      {open && (
        <form
          className="mt-3 space-y-4 border-t border-line pt-3"
          noValidate
          onSubmit={(event) => {
            event.preventDefault();
            const proof = reauth.proof();
            if (proof) unlink.mutate({ provider: identity.provider_id, proof }, { onSettled: reauth.clear });
          }}
        >
          <p className="text-[13px] text-fg-muted">You can no longer sign in with {name}. Unlinking signs out your other sessions.</p>
          <ReauthFields id={id} reauth={reauth} />
          {unlink.isError && (
            <Notice tone="error" title={refused ? "This sign-in method is still needed" : `Could not unlink ${name}`}>
              {refused ? unlinkRefusal(unlink.error) : reauthErrorMessage(unlink.error, reauth.method)}
            </Notice>
          )}
          <div className="flex flex-wrap justify-end gap-2">
            <Button variant="ghost" onClick={() => { setOpen(false); unlink.reset(); }} disabled={unlink.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="danger" disabled={!online} pending={unlink.isPending}>
              Unlink {name}
            </Button>
          </div>
        </form>
      )}
    </li>
  );
}

function identityName(identity: LinkedIdentity, providers: readonly SignInProvider[], directories: readonly PublicLdapProvider[]) {
  if (identity.provider_id === "github") return "GitHub";
  const directory = ldapProviderId(identity.provider_id);
  if (directory !== null) return directories.find((item) => item.id === directory)?.display_name ?? "LDAP directory (turned off)";
  return providers.find((provider) => provider.id === identity.provider_id)?.display_name ?? identity.provider_id;
}

function LinkedIdentities({ providers, directories }: { providers: readonly SignInProvider[]; directories: readonly PublicLdapProvider[] }) {
  const identities = useQuery(identitiesQuery);
  const factors = useQuery(accountSignInMethodsQuery);
  if (identities.isPending || factors.isPending) return <LoadingRows label="Loading linked identities" rows={1} />;
  if (identities.isError || factors.isError) {
    const failed = identities.isError ? identities : factors;
    return (
      <div className="p-3">
        <QueryError what="linked identities" error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
      </div>
    );
  }
  if (identities.data.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No identity provider, directory or GitHub accounts are linked.</p>;
  return (
    <ul className="divide-y divide-line">
      {identities.data.map((identity) => (
        <IdentityRow
          key={`${identity.provider_id}:${identity.subject}`}
          identity={identity}
          name={identityName(identity, providers, directories)}
          factors={factors.data}
        />
      ))}
    </ul>
  );
}

function LinkProvider({ providers }: { providers: readonly SignInProvider[] }) {
  const id = useId();
  const session = useSession();
  const online = useOnline();
  const start = useOidcStart();
  const [provider, setProvider] = useState("");
  const [password, setPassword] = useState("");
  const first = providers.at(0);
  if (!first) return null;
  const selected = provider || first.id;
  const needsPassword = session?.has_password ?? true;
  return (
    <form
      className="min-w-0 space-y-4 border-t border-line p-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (needsPassword && password === "") return;
        start.mutate({ provider: selected, link: true, current_password: needsPassword ? password : "" }, { onSettled: () => setPassword("") });
      }}
    >
      <div>
        <h3 className="text-[13px] font-semibold text-fg">Link an identity provider</h3>
        <p className="mt-0.5 text-xs text-fg-muted">
          The provider must confirm a verified email address.
          {!needsPassword && " Because this account has no password, linking only works within ten minutes of signing in; sign in again if it is refused."}
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        {providers.length > 1 ? (
          <Field id={`${id}-provider`} label="Provider">
            <Select id={`${id}-provider`} className={inputClass} value={selected} onChange={(event) => setProvider(event.target.value)}>
              {providers.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.display_name}
                </option>
              ))}
            </Select>
          </Field>
        ) : (
          <div className="space-y-1.5">
            <p className="text-[13px] font-medium text-fg">Provider</p>
            <p className="text-[13px] text-fg-muted">{first.display_name}</p>
          </div>
        )}
        {needsPassword && <CurrentPasswordField id={`${id}-password`} value={password} onChange={setPassword} />}
      </div>
      {start.isError && <Notice tone="error" title="Could not start linking">{errorMessage(start.error)}</Notice>}
      <div className="flex justify-end">
        <Button type="submit" disabled={!online || (needsPassword && password === "")} pending={start.isPending || start.isSuccess}>
          Link account
        </Button>
      </div>
    </form>
  );
}

function LinkDirectory({ directories }: { directories: readonly PublicLdapProvider[] }) {
  const id = useId();
  const online = useOnline();
  const session = useSession();
  const factors = useQuery(accountSignInMethodsQuery);
  const link = useLinkLdapIdentity();
  const reauth = useReauth(factors.data ?? { totp_enrolled: false, recovery_codes_remaining: 0 });
  const [provider, setProvider] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const first = directories.at(0);
  if (!first) return null;
  const selected = directories.find((item) => item.id === provider) ?? first;
  const linkError = link.isError ? (isStatus(link.error, 403) || isStatus(link.error, 429) ? reauthErrorMessage(link.error, reauth.method) : ldapSignInErrorMessage(link.error, errorMessage)) : null;
  return (
    <form
      className="min-w-0 space-y-4 border-t border-line p-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const missing = username.trim() === "" || password === "";
        setError(missing ? "Enter your directory username and password." : undefined);
        const proof = reauth.proof();
        if (missing || !proof) return;
        link.mutate(
          { provider: selected.id, username: username.trim(), password, proof },
          {
            onSuccess: () => setUsername(""),
            onSettled: () => {
              setPassword("");
              reauth.clear();
            },
          },
        );
      }}
    >
      <div>
        <h3 className="text-[13px] font-semibold text-fg">Link a directory account</h3>
        <p className="mt-0.5 text-xs text-fg-muted">
          The directory account's email must match this account's email{session?.email ? ` (${session.email})` : ""}. Team access from directory groups is applied when you link it and each time you sign in with it.
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        {directories.length > 1 ? (
          <Field id={`${id}-directory`} label="Directory">
            <Select id={`${id}-directory`} className={inputClass} value={selected.id} onChange={(event) => setProvider(event.target.value)}>
              {directories.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.display_name}
                </option>
              ))}
            </Select>
          </Field>
        ) : (
          <div className="space-y-1.5 sm:col-span-2">
            <p className="text-[13px] font-medium text-fg">Directory</p>
            <p className="text-[13px] text-fg-muted">{first.display_name}</p>
          </div>
        )}
        <Field id={`${id}-username`} label="Directory username">
          <input id={`${id}-username`} className={inputClass} autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={254} value={username} onChange={(event) => setUsername(event.target.value)} />
        </Field>
        <Field id={`${id}-password`} label="Directory password" error={error}>
          <input
            id={`${id}-password`}
            className={inputClass}
            type="password"
            autoComplete="off"
            maxLength={4096}
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy(`${id}-password`, error)}
          />
        </Field>
      </div>
      <ReauthFields id={`${id}-proof`} reauth={reauth} />
      {linkError && <Notice tone="error" title="Could not link the directory account">{linkError}</Notice>}
      {link.isSuccess && <Notice tone="success" title="Directory account linked">You can now sign in with your directory username and password.</Notice>}
      <div className="flex justify-end">
        <Button type="submit" disabled={!online} pending={link.isPending}>
          Link directory account
        </Button>
      </div>
    </form>
  );
}

export function SignInIdentities() {
  const methods = useQuery(signInMethodsQuery);
  const directories = useQuery(publicLdapProvidersQuery);
  const providers = methods.data?.oidc_providers ?? [];
  return (
    <Section
      title="Sign-in identities"
      description="Identity provider, directory and GitHub accounts you can sign in with. Accounts are never linked automatically by email address."
      width="form"
      flush
    >
      <LinkedIdentities providers={providers} directories={directories.data ?? []} />
      {methods.isError ? (
        <div className="border-t border-line p-3">
          <QueryError what="single sign-on providers" error={methods.error} onRetry={() => void methods.refetch()} retrying={methods.isFetching} />
        </div>
      ) : (
        <LinkProvider providers={providers} />
      )}
      <LinkDirectory directories={directories.data ?? []} />
    </Section>
  );
}
