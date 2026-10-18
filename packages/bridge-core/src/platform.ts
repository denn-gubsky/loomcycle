// What a platform can do, so the core picks how to deliver a turn.

export interface PlatformInfo {
  /** Lower-case name, the prefix of every subject this bridge creates: "teams". */
  name: string;
  /** How people call the platform, in messages: "Teams". */
  label: string;
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
