import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
  localStorage.clear();
  sessionStorage.clear();
});

// jsdom has no layout. Give elements a size so the windowed log viewer renders
// rows, and stub the scrolling APIs it calls.
Object.defineProperty(HTMLElement.prototype, "getBoundingClientRect", {
  configurable: true,
  value: () => ({ x: 0, y: 0, top: 0, left: 0, bottom: 400, right: 800, width: 800, height: 400, toJSON: () => ({}) }),
});
Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, get: () => 400 });
Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => 800 });
Object.defineProperty(Element.prototype, "clientHeight", { configurable: true, get: () => 400 });
Object.defineProperty(Element.prototype, "clientWidth", { configurable: true, get: () => 800 });
Element.prototype.scrollTo = function scrollTo() {};
Element.prototype.scrollIntoView = function scrollIntoView() {};

if (typeof globalThis.ResizeObserver === "undefined") {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
}
