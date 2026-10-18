#!/usr/bin/env node
import { runCli } from "@loomcycle/bridge-core";

import { createSlackBridge } from "./bridge.js";
import { loadConfig } from "./config.js";

void runCli("slack-bridge", () => createSlackBridge(loadConfig()));
