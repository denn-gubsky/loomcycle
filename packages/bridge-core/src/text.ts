// Small text helpers: commands, splitting long replies.

export type Command = "new" | "stop" | "help" | "voice" | "voice on" | "voice off" | "voice auto";

/** parseCommand recognises the bridge's own commands: the whole message,
 *  case-insensitive, with any run of spaces between words. Anything else goes
 *  to the agent. */
export function parseCommand(text: string): Command | undefined {
  const t = text.trim().toLowerCase().replace(/\s+/g, " ");
  if (t === "/new") return "new";
  if (t === "/stop") return "stop";
  if (t === "/help") return "help";
  if (t === "/voice") return "voice";
  if (t === "/voice on") return "voice on";
  if (t === "/voice off") return "voice off";
  if (t === "/voice auto") return "voice auto";
  return undefined;
}

export const HELP_TEXT = [
  "Commands:",
  "- **/new**: start a new conversation (the agent forgets this one)",
  "- **/stop**: stop the current answer",
  "- **/help**: show this list",
].join("\n");

/** The /voice lines, added to /help on a bridge that takes voice messages. */
export const VOICE_HELP_TEXT = [
  "- **/voice on**: always answer with a voice message as well",
  "- **/voice off**: answer in text only",
  "- **/voice auto**: answer a voice message with one (the default)",
].join("\n");

/** splitMessage cuts text into pieces of at most max characters, at a line
 *  break when one is near, so a long answer arrives as several messages. */
export function splitMessage(text: string, max: number): string[] {
  const out: string[] = [];
  let rest = text;
  while (rest.length > max) {
    let cut = rest.lastIndexOf("\n", max);
    if (cut < max / 2) cut = max;
    out.push(rest.slice(0, cut));
    rest = rest.slice(cut).replace(/^\n/, "");
  }
  if (rest.length > 0) out.push(rest);
  return out;
}

/** trimTrailingSlashes drops every "/" at the end of a URL. A loop, not
 *  /\/+$/: that regex is quadratic on a long run of slashes not at the end. */
export function trimTrailingSlashes(url: string): string {
  let end = url.length;
  while (end > 0 && url[end - 1] === "/") end--;
  return url.slice(0, end);
}
