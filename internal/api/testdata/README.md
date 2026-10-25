`profile-wall.collapsed` is **constructed**, not recorded: no collapsed output
from the node exists yet. The stacks are the ones of `testdata/mpa-threads.txt`
in async-profiler's collapsed format (`-e wall -t`, 10 s, 50 ms), matching the
node test (`Server thread` 201 samples in `MinecraftServer.waitUntilNextTick`).
Replace it with a real recording in milestone 3.
