import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

let nextId = 0;

/** Unique ids for list rows that have no server identity yet. */
export function localId(prefix = "id"): string {
  nextId += 1;
  return `${prefix}-${nextId}`;
}
