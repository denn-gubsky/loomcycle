#!/usr/bin/env node
// loomcycle-bridge-teams: run the bridge from environment variables (RFC EN §8).

import { runCli } from "@loomcycle/bridge-core";

import { createTeamsBridge } from "./bridge.js";
import { loadConfig } from "./config.js";

void runCli("teams-bridge", () => createTeamsBridge(loadConfig()) as never);
