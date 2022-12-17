`mgd` is an experimental Linux tool which combines multiple servers into a single whole that is greater than the sum of its parts. In an ideal world, I'd be able to install servers around the world, each running a single daemon program (`mgd`), and they'd automatically start talking to each other, working together, reacting to failures, load balancing requests, all that juicy stuff.

I'll be streaming the development of `mgd` in the new year on my YT (soon), likely on Saturdays.

Small note: This project is mostly a pipedream, it's probably not possible, but it seems like a fun, interesting challenge to tackle and see what comes out of it. There's been some new tech released recently that I think have made these truly autonomous, homogeneous systems possible, but frankly, I'm not entirely sure.

Some fundamental principles behind the design:
- Minimal dependency on external services (ideally, zero).
- No pre-defined "leader" server.
- Each server running `mgd` operates autonomously.
- Servers can be added or removed from the network at any time.
- All machines contribute whatever resources they can to the network.
- Every machine runs every service.
- Best-effort approach wherever possible.

Resistance is futile.

Roadmap

- Fault-tolerant storage system (filesystem + database)
- External networking
- App deployment + hosting
- ??
