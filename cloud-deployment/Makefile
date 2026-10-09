# Wrapper so you never have to remember the two --env-file flags (compose needs
# both files for `${VAR}` interpolation). Run from the deployment directory.
# versions.env is optional: it holds the image versions and the loomboard ref when
# the deploy script manages them (deploy/loomcycle-cloud-deploy). Without it the
# defaults in docker-compose.yaml apply.
ENVFILES := --env-file .env.insecure --env-file .env.secure $(if $(wildcard versions.env),--env-file versions.env)
COMPOSE  := docker compose $(ENVFILES)

.PHONY: up down restart logs ps pull config migrate psql
up:      ## start the whole stack
	$(COMPOSE) up -d
down:    ## stop + remove containers (volumes/data persist)
	$(COMPOSE) down
restart: ## restart all services
	$(COMPOSE) restart
logs:    ## follow logs (make logs S=loomcycle for one service)
	$(COMPOSE) logs -f --tail=200 $(S)
ps:      ## service status
	$(COMPOSE) ps
pull:    ## pull newer images
	$(COMPOSE) pull
config:  ## render + validate the merged compose
	$(COMPOSE) config
migrate: ## re-run the one-shot schema migration
	$(COMPOSE) run --rm loomcycle-migrate
psql:    ## open a psql shell on the main db
	$(COMPOSE) exec postgres psql -U loomcycle -d loomcycle
