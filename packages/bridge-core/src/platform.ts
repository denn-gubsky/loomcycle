// What a platform can do, so the core picks how to deliver a turn.

export interface PlatformInfo {
  /** Lower-case name, the prefix of every subject this bridge creates: "teams". */
  name: string;
  /** How people call the platform, in messages: "Teams". */
  label: string;
  /** What /help answers with, for a platform with commands or setup of its
   *  own. Unset or empty, /help answers with the core's HELP_TEXT. To add to
   *  that list rather than copy it, build on the exported HELP_TEXT. */
  helpText?: string;
  /** Set on a platform where every voice message must be answered, even in
   *  a shared chat: one whose voice arrives as an interaction it has already
   *  acknowledged (Discord's "Ask by voice"), which otherwise waits out its
   *  token. Elsewhere a shared chat stays quiet when a voice message is not
   *  taken, as it does for a photo (RFC GA). */
  answerEveryVoice?: boolean;
}

export interface PlatformCapabilities {
  /** Where a turn is streamed as it is written. Elsewhere a turn is sent as
   *  one message when it ends, after a typing indicator. Streaming also needs
   *  the port's `createStream`. */
  streaming: "always" | "personal-only" | "never";
  /** Longest message the platform takes, in characters; a longer answer is
   *  split across messages. */
  maxMessageChars: number;
}
