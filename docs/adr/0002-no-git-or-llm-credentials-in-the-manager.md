# The manager handles no git or LLM credentials

An earlier version of the manager issued a LiteLLM key for every task and ran a git/MCP gateway that held a GitLab administrator token. Both were removed. The company provides its own git MCP and LLM gateway as sidecars that agents use directly, so the manager is only responsible for lending pods. Until those sidecars are in place, a pod reaches an LLM and runs git clone with the agent's own custom environment variables in Multica, which the stock daemon already passes to the agent.
