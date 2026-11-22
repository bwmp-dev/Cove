import { useLayoutEffect, type RefObject } from "react";

export function useFloatingPanel({ open, id, trigger, panel, onClose, align = "start", width }: {
  open: boolean;
  id: string;
  trigger: RefObject<HTMLElement | null>;
  panel: RefObject<HTMLElement | null>;
  onClose: () => void;
  align?: "start" | "end";
  width?: number;
}) {
  useLayoutEffect(() => {
    if (!open || !panel.current || !trigger.current) return;
    const element = panel.current;
    const anchorElement = trigger.current;
    // Position through a nonce-authorized sheet, without weakening the CSP for inline styles.
    const sheet = document.createElement("style");
    sheet.nonce = document.querySelector<HTMLMetaElement>('meta[name="cove-style-nonce"]')?.content ?? "";
    document.head.append(sheet);
    function position() {
      const viewport = window.visualViewport;
      const left = (viewport?.offsetLeft ?? 0) + 8;
      const top = (viewport?.offsetTop ?? 0) + 8;
      const viewportWidth = Math.max(0, (viewport?.width ?? window.innerWidth) - 16);
      const viewportHeight = Math.max(0, (viewport?.height ?? window.innerHeight) - 16);
      const anchor = anchorElement.getBoundingClientRect();
      const below = Math.max(0, top + viewportHeight - anchor.bottom - 6);
      const above = Math.max(0, anchor.top - top - 6);
      const upward = element.scrollHeight > below && above > below;
      const available = Math.min(320, viewportHeight, upward ? above : below);
      const panelWidth = Math.min(width ?? Math.max(anchor.width, 200), viewportWidth);
      const panelHeight = Math.min(element.scrollHeight + 2, available);
      const x = Math.max(left, Math.min(align === "end" ? anchor.right - panelWidth : anchor.left, left + viewportWidth - panelWidth));
      const y = Math.max(top, Math.min(upward ? anchor.top - 6 - panelHeight : anchor.bottom + 6, top + viewportHeight - panelHeight));
      sheet.textContent = `[id="${id}"]{left:${x}px;top:${y}px;width:${panelWidth}px;max-height:${available}px;visibility:visible}`;
    }
    function outside(event: PointerEvent) {
      if (event.target instanceof Node && !element.contains(event.target) && !anchorElement.contains(event.target)) onClose();
    }
    position();
    const observer = new ResizeObserver(position);
    observer.observe(element);
    observer.observe(anchorElement);
    window.addEventListener("resize", position);
    window.addEventListener("scroll", position, true);
    window.visualViewport?.addEventListener("resize", position);
    window.visualViewport?.addEventListener("scroll", position);
    document.addEventListener("pointerdown", outside);
    return () => {
      observer.disconnect();
      sheet.remove();
      window.removeEventListener("resize", position);
      window.removeEventListener("scroll", position, true);
      window.visualViewport?.removeEventListener("resize", position);
      window.visualViewport?.removeEventListener("scroll", position);
      document.removeEventListener("pointerdown", outside);
    };
  }, [open, id, trigger, panel, onClose, align, width]);
}

export const floatingPanelClass = "invisible fixed z-50 overflow-y-auto overscroll-contain rounded-lg border border-line-strong bg-raised p-1 shadow-xl";
