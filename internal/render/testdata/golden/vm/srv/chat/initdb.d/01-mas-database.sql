-- Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
--
-- Matrix Authentication Service keeps its own database beside the
-- homeserver's. They cannot share one: both define a `users` table.
--
-- RUNS EXACTLY ONCE, on an empty data directory, in the same pass that applies
-- POSTGRES_INITDB_ARGS. A database that already has data will never see this
-- file, so adding MAS to a stack that has been up before means creating this
-- database by hand.
CREATE DATABASE chat_mas OWNER chat;
