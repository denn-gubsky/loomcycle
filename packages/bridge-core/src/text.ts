// Small text helpers: commands, splitting long replies.

export type Command = "new" | "stop" | "help";

/** parseCommand recognises the bridge's own commands: the whole message,
 *  case-insensitive. Anything else goes to the agent. */
export function parseCommand(text: string): Command | undefined {
  const t = text.trim().toLowerCase();
  if (t === "/new") return "new";
  if (t === "/stop") return "stop";
  if (t === "/help") return "help";
  return undefined;
}

export const HELP_TEXT = [
  "Commands:",
  "- **/new**: start a new conversation (the agent forgets this one)",
  "- **/stop**: stop the current answer",
  "- **/help**: show this list",
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
