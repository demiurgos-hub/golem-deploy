import { useEffect, useState } from "react";
import { Check, Copy } from "lucide-react";
import { Button, type ButtonProps } from "@/components/ui/button";

async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Fall back below.
  }
  // Plain-HTTP deployments have no async clipboard API.
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  let ok: boolean;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  ta.remove();
  return ok;
}

export function CopyButton({
  value,
  label = "Copy",
  size = "sm",
  variant = "outline",
}: {
  value: string;
  label?: string;
  size?: ButtonProps["size"];
  variant?: ButtonProps["variant"];
}) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");

  useEffect(() => {
    if (state === "idle") return;
    const t = setTimeout(() => setState("idle"), 2000);
    return () => clearTimeout(t);
  }, [state]);

  const text = state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : label;
  return (
    <Button
      size={size}
      variant={variant}
      onClick={() => void copyText(value).then((ok) => setState(ok ? "copied" : "failed"))}
      aria-label={size === "icon" ? text : undefined}
      title={size === "icon" ? text : undefined}
    >
      {state === "copied" ? <Check /> : <Copy />}
      {size !== "icon" && <span aria-live="polite">{text}</span>}
    </Button>
  );
}
