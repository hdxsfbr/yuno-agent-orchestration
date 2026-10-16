# oh-my-mermaid CLI and viewer (architecture diagrams under .omm/), so nothing is
# installed on the host. Built by `make omm-view` or `scripts/omm.sh`.
FROM node:24-slim
RUN npm install -g oh-my-mermaid@0.2.0 && npm cache clean --force
WORKDIR /work
ENTRYPOINT ["omm"]
