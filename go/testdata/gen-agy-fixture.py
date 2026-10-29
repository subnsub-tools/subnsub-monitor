#!/usr/bin/env python3
"""Antigravity conversation databases for tokens_agy_test.go, synthetic from
end to end: the tables and protobuf field numbers Antigravity's CLI writes
(see tokens_agy.go), made-up counts, ids and names.

  agy-plain.db     rollback journal, 4096-byte pages: interior pages, an
                   overflowing row, a failed call, a repeated response id, a
                   call that names only its model's enum
  agy-more.db      agy-plain.db and two calls after it: one new, one repeating
                   a response id already counted
  agy-wal.db(-wal) WAL mode, its calls only in the -wal file

Run from this directory: python3 gen-agy-fixture.py
"""
import os
import shutil
import sqlite3

T0 = 1793200000  # 2026-10-28T13:46:40Z


def varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def field_v(num, v):
    return varint(num << 3) + varint(v)


def field_b(num, b):
    if isinstance(b, str):
        b = b.encode()
    return varint(num << 3 | 2) + varint(len(b)) + b


def ts(sec):
    return field_v(1, sec)


def gen(enum, rid=None, model=None, inp=0, out=None, cw=0, cr=0, think=0, resp=0,
        steps=(), created=None, error=None, prompt=0):
    usage = field_v(1, enum)
    for num, v in ((2, inp), (4, cw), (5, cr), (9, think), (10, resp)):
        if v:
            usage += field_v(num, v)
    if out is not None:
        usage += field_v(3, out)
    if rid:
        usage += field_b(7, b"m" * 40) + field_b(11, rid)
    chat = b""
    if prompt:
        chat += field_b(1, b"p" * prompt)
    chat += field_v(3, enum) + field_b(4, usage)
    if created is not None:
        chat += field_b(9, field_b(4, ts(created)))
    if model:
        chat += field_b(19, model)
    top = b""
    if steps:
        top += field_b(2, b"".join(varint(s) for s in steps))
    top += field_b(4, b"exec-1")
    if error:
        top += field_b(5, error)
    return top + field_b(1, chat)


def schema(db):
    db.executescript("""
    CREATE TABLE `trajectory_meta` (`trajectory_id` text,`cascade_id` text,`trajectory_type` integer,`source` integer,PRIMARY KEY (`trajectory_id`));
    CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`has_subtrajectory` numeric NOT NULL DEFAULT false,`metadata` blob,`error_details` blob,`permissions` blob,PRIMARY KEY (`idx`));
    CREATE TABLE `gen_metadata` (`idx` integer,`data` blob,`size` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));
    CREATE TABLE `trajectory_metadata_blob` (`id` text DEFAULT "main",`data` blob,PRIMARY KEY (`id`));
    """)
    db.execute("INSERT INTO trajectory_metadata_blob VALUES ('main', ?)", (field_b(2, ts(T0 - 600)),))


def put_gen(db, idx, data):
    db.execute("INSERT INTO gen_metadata VALUES (?, ?, ?)", (idx, data, len(data)))


def put_step(db, idx, created):
    db.execute("INSERT INTO steps (idx, step_type, metadata) VALUES (?, 15, ?)", (idx, field_b(1, ts(created))))


def plain():
    for p in ("agy-plain.db", "agy-plain.db-journal"):
        if os.path.exists(p):
            os.remove(p)
    db = sqlite3.connect("agy-plain.db")
    db.execute("PRAGMA page_size=4096")
    schema(db)
    # 0: timed by its planner step; thinking inside output.
    put_gen(db, 0, gen(1318, "resp-0", "gemini-3.8-flash", inp=13896, out=158, think=117, resp=41, steps=(1, 2)))
    put_step(db, 1, T0)
    # 1: a call that failed: no response id, its enum alone in its usage.
    put_gen(db, 1, gen(1417, error="quota", steps=()))
    # 2: names only its enum; timed by its own chat start.
    put_gen(db, 2, gen(1318, "resp-2", None, inp=2810, out=186, cr=20353, think=111, resp=75, created=T0 + 60))
    # 3: resp-0 again.
    put_gen(db, 3, gen(1318, "resp-0", "gemini-3.8-flash", inp=13896, out=158, think=117, resp=41, steps=(1,)))
    # 4: a prompt big enough to spill onto overflow pages; a cache write.
    put_gen(db, 4, gen(1417, "resp-4", "gemini-4-argon-c", inp=5, out=10, cw=7, think=4, resp=6, steps=(9,), prompt=20000))
    put_step(db, 9, T0 + 120)
    # 5..304: enough rows for interior pages, each timed by the conversation.
    for i in range(5, 305):
        put_gen(db, i, gen(1298, "resp-%d" % i, "gemini-3.7-flash", inp=1, out=2, think=1, resp=1, prompt=200))
    db.commit()
    db.close()


def more():
    shutil.copyfile("agy-plain.db", "agy-more.db")
    db = sqlite3.connect("agy-more.db")
    put_gen(db, 305, gen(1318, "resp-305", "gemini-3.8-flash", inp=7, out=9, think=3, resp=6, created=T0 + 900))
    put_gen(db, 306, gen(1318, "resp-2", None, inp=2810, out=186, cr=20353, think=111, resp=75, created=T0 + 60))
    db.commit()
    db.close()


def wal():
    for p in ("agy-wal.db", "agy-wal.db-wal", "agy-wal.db-shm", "work.db", "work.db-wal", "work.db-shm"):
        if os.path.exists(p):
            os.remove(p)
    db = sqlite3.connect("work.db")
    db.execute("PRAGMA page_size=4096")
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA wal_autocheckpoint=0")
    schema(db)
    db.commit()
    db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    for i in range(3):
        put_gen(db, i, gen(1318, "wal-%d" % i, "gemini-3.8-flash-n", inp=100 + i, out=20, think=5, resp=15, created=T0 + 300 + i))
        db.commit()
    # Copied while the connection holds the log open, before any checkpoint.
    shutil.copyfile("work.db", "agy-wal.db")
    shutil.copyfile("work.db-wal", "agy-wal.db-wal")
    db.close()
    for p in ("work.db", "work.db-wal", "work.db-shm"):
        if os.path.exists(p):
            os.remove(p)


if __name__ == "__main__":
    plain()
    more()
    wal()
