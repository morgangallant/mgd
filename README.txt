`mgd` is a Linux tool which can be used to deploy containers onto servers with very little hassle. It is mostly an experiment to learn about linux containerization and orchestration. You should not use this in production, rather, you should use Railway (https://railway.app).

Some fundamental principles behind the design:
- No dependency on external services.
- No pre-defined "leader" server.
- Each server running `mgd` operates autonomously.
- Servers can be added or removed at any time.
- Machines contribute whatever resources they can to the network.

Resistance is futile.