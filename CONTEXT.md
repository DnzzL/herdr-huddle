# Herdr Huddle

A shared thread that lets a second person watch and steer one coding agent that runs in an agent session on the operator's machine, with GitHub keeping the record.

## Language

**Share**:
The act and record of making one agent's session followable: a branch, a pull request, and a bound origin on the operator's machine. A thread is dead once its share is retired.

**Thread**:
The pull request: its body is a static header, and its comments are the recording of the conversation.

**Turn**:
Everything one agent did between two moments of rest. The unit that gets posted, and the anchor a comment can attach to.

**Operator**:
The person whose machine and agent session a share is bound to. Runs the poller; owns the permission gate.

**Collaborator**:
A person an operator invites to a share. Joins the operator's machine-bound world and runs nothing of their own — no agent, no checkout, no Herdr. Explicitly not a peer who brings their own environment.

**Allowlist**:
The set of logins whose comments may be delivered into the agent. The real security boundary of the thread.

_Avoid_: peer, teammate (they imply a separate environment, which is out of scope)

**Stream**:
The one-way flow of the agent's ANSI frames, from the pane to the joiners. Read-only by construction; the collaborator sends nothing into it. One per joiner, rendered at that joiner's own terminal size.

**Huddle**:
A share's live half while `serve` is running: the stream, the people in it, and the agent's state, as one thing. The room a joiner is in, as opposed to the pipe they are reading.

**Door**:
How somebody who is not yet on the allowlist gets in. Three settings and no more: knock (the operator is asked, and their answer is written to the allowlist), open (any proven GitHub identity), closed (the allowlist only). Not a secret, not a code — always a login.

_Avoid_: join code, invite token (they replace a person with a secret, and the thread's attribution is the product)

**Viewer**:
The role of the client a collaborator runs to join a live share — it renders the stream and sends instructions. Its proof of identity is the collaborator's GitHub token. The command name is `join`, not `watch`: the client has a voice.

_Avoid_: watch (a read-only word for a client that can steer)

**Ledger**:
GitHub. The canonical, append-only record of every turn and every steering comment. The transport must never wait for it; it must eventually hold everything.

_Avoid_: live view (the act of watching, not an entity), feed, relay
