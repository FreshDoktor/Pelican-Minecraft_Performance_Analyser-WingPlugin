`profile-wall.collapsed` is **constructed**, not recorded: no collapsed output
from the node exists yet. The stacks are the ones of `testdata/mpa-threads.txt`
in async-profiler's collapsed format (`-e wall -t`, 10 s, 50 ms), matching the
node test (`Server thread` 201 samples in `MinecraftServer.waitUntilNextTick`).
Replace it with a recording from the node once one exists (the WSL test of
milestone 3 confirmed the format against a real JVM).
