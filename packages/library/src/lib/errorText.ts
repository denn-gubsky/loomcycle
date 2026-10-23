import { createContext, useCallback, useContext } from "react";

// ErrorFormatter is the host's wording for an error it recognizes, or
// undefined to keep the Library's own. The Library cannot parse every refusal
// the runtime writes (an access-control denial names the action, the resource
// and the rule that decided), and a host that already does — the loomcycle Web
// UI — should not have its pages and the Library read differently. A host
// without one passes nothing and gets the error's message, as before.
export type ErrorFormatter = (error: unknown) => string | undefined;

export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// describeError is the text an error renders as: the host's wording when it
// has one, else `fallback` (the error's message, or a site's own explanation).
export function describeError(
  e: unknown,
  format: ErrorFormatter | undefined,
  fallback: (e: unknown) => string = errorMessage,
): string {
  return format?.(e) ?? fallback(e);
}

// Through context rather than a prop on every panel and modal, as the data
// layer is.
const ErrorFormatterContext = createContext<ErrorFormatter | undefined>(undefined);

export const ErrorFormatterProvider = ErrorFormatterContext.Provider;

export function useErrorText(): (e: unknown, fallback?: (e: unknown) => string) => string {
  const format = useContext(ErrorFormatterContext);
  return useCallback((e, fallback) => describeError(e, format, fallback), [format]);
}
