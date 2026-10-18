#!/usr/bin/env node
import { runCli } from "@loomcycle/bridge-core";

import { createDiscordBridge } from "./bridge.js";
import { loadConfig } from "./config.js";

void runCli("discord-bridge", () => createDiscordBridge(loadConfig()));
