import { useQuery } from "@tanstack/react-query";
import { Pin, PinOff } from "lucide-react";
import { errorMessage } from "../api/client";
import { pinsQuery, useSetPin, type PinKind } from "../api/organization";
import { Button } from "./ui";

// A personal favourite for the current user in the selected team; it changes nothing for anyone else.
export function PinButton({ kind, id, name, size = "md" }: { kind: PinKind; id: string; name: string; size?: "sm" | "md" }) {
  const pins = useQuery(pinsQuery);
  const setPin = useSetPin();
  const pinned = pins.data?.some((pin) => pin.kind === kind && pin.id === id) ?? false;
  const label = pinned ? `Unpin ${name}` : `Pin ${name}`;
  return (
    <>
    <Button
      size={size}
      aria-label={label}
      title={label}
      disabled={pins.isPending}
      pending={setPin.isPending}
      onClick={() => setPin.mutate({ kind, id, pinned: !pinned })}
    >
      {!setPin.isPending && (pinned ? <PinOff aria-hidden className="size-3.5" /> : <Pin aria-hidden className="size-3.5" />)}
      {pinned ? "Unpin" : "Pin"}
    </Button>
      {setPin.isError && (
        <span role="alert" className="text-xs text-bad">
          {errorMessage(setPin.error)}
        </span>
      )}
    </>
  );
}
