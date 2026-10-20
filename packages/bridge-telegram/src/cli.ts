#!/usr/bin/env node
// loomcycle-bridge-telegram: run the bridge from environment variables.

import { runCli } from "@loomcycle/bridge-core";

import { createTelegramBridge } from "./bridge.js";
import { loadConfig } from "./config.js";

void runCli("telegram-bridge", () => createTelegramBridge(loadConfig()));
