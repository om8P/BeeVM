"""Builds the BeeVM micro project report from the department template (python-docx).

    python3 report/tools/build_report.py

Keeps the template's cover page, header, styles and table look (Nunito body text, Times New Roman
headings, black-bordered tables with a light blue header row) and replaces the guidance pages with
the report. Inputs: the template .docx, images in report/assets/, benchmark charts in
bench/results/m12/<run>/. Output: report/BeeVM_Micro_Project_Report.docx
"""
import copy
import os
import re

import docx
from docx.enum.table import WD_TABLE_ALIGNMENT
from docx.enum.text import WD_ALIGN_PARAGRAPH, WD_BREAK
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Cm, Pt, RGBColor
from docx.table import Table
from docx.text.paragraph import Paragraph

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
TEMPLATE = os.path.expanduser(r"~\Downloads\Common_Micro_Project_Report_Template_Computer_Engineering (1).docx")
A = os.path.join(ROOT, "report", "assets")
BENCH = os.path.join(ROOT, "bench", "results", "m12", "20261117-090325")
ARCH = os.path.join(ROOT, "developer-guide", "2BeeVM system architecture@2x.png")
OUT = os.path.join(ROOT, "report", "BeeVM_Micro_Project_Report.docx")

FONT, MONO = "Nunito", "Consolas"
BODY, SMALL, TBL = 11, 9.5, 9
HEAD_FILL = "D9EAF7"

d = docx.Document(TEMPLATE)
body = d.element.body
els = list(body.iterchildren())

# ---------------------------------------------------------------- helpers


def set_run_font(run, size=BODY, bold=None, italic=None, font=FONT, color=None):
    run.font.name = font
    rpr = run._r.get_or_add_rPr()
    rfonts = rpr.find(qn("w:rFonts"))
    if rfonts is None:
        rfonts = OxmlElement("w:rFonts")
        rpr.insert(0, rfonts)
    for a in ("w:ascii", "w:hAnsi", "w:cs", "w:eastAsia"):
        rfonts.set(qn(a), font)
    run.font.size = Pt(size)
    if bold is not None:
        run.font.bold = bold
    if italic is not None:
        run.font.italic = italic
    if color:
        run.font.color.rgb = RGBColor.from_string(color)


TOKEN = re.compile(r"(\*\*[^*]+\*\*|`[^`]+`)")


def rich(p, text, size=BODY, bold=False, italic=False, color=None):
    """Adds text with **bold** and `code` markup to paragraph p."""
    for part in TOKEN.split(text):
        if not part:
            continue
        if part.startswith("**") and part.endswith("**"):
            r = p.add_run(part[2:-2])
            set_run_font(r, size, True, italic, color=color)
        elif part.startswith("`") and part.endswith("`"):
            r = p.add_run(part[1:-1])
            set_run_font(r, size - 1, bold, italic, font=MONO, color="1F3A5F")
        else:
            r = p.add_run(part)
            set_run_font(r, size, bold, italic, color=color)
    return p


def fmt(p, after=6, before=0, line=1.15, align=WD_ALIGN_PARAGRAPH.JUSTIFY, keep_next=False):
    pf = p.paragraph_format
    pf.space_after = Pt(after)
    pf.space_before = Pt(before)
    pf.line_spacing = line
    pf.keep_with_next = keep_next
    if align is not None:
        p.alignment = align
    return p


def para(text, size=BODY, after=6, align=WD_ALIGN_PARAGRAPH.JUSTIFY, bold=False, italic=False, keep_next=False):
    p = d.add_paragraph(style="normal")
    rich(p, text, size, bold, italic)
    return fmt(p, after=after, align=align, keep_next=keep_next)


BULLET_NUMPR = None
for el in els[14:22]:
    np_ = el.find(".//" + qn("w:numPr"))
    if np_ is not None:
        BULLET_NUMPR = copy.deepcopy(np_)
        break


def bullet(text, size=BODY, level=0):
    p = d.add_paragraph(style="normal")
    if BULLET_NUMPR is not None and level == 0:
        p._p.get_or_add_pPr().append(copy.deepcopy(BULLET_NUMPR))
        p.paragraph_format.left_indent = Cm(0.63)
        p.paragraph_format.first_line_indent = Cm(-0.63)
    else:
        p.paragraph_format.left_indent = Cm(0.63 + 0.63 * level)
        p.paragraph_format.first_line_indent = Cm(-0.4)
        r = p.add_run("–  " if level else "•  ")
        set_run_font(r, size)
    rich(p, text, size)
    return fmt(p, after=3)


def numbered(items, size=BODY):
    for i, t in enumerate(items, 1):
        p = d.add_paragraph(style="normal")
        p.paragraph_format.left_indent = Cm(0.75)
        p.paragraph_format.first_line_indent = Cm(-0.6)
        r = p.add_run("%d.  " % i)
        set_run_font(r, size, True)
        rich(p, t, size)
        fmt(p, after=3)


def h1(text, page_break=True):
    p = d.add_paragraph(text, style="Heading 1")
    p.paragraph_format.page_break_before = page_break
    p.paragraph_format.space_after = Pt(8)
    return p


def h2(text):
    p = d.add_paragraph(text, style="Heading 2")
    p.paragraph_format.space_before = Pt(10)
    p.paragraph_format.space_after = Pt(5)
    return p


def h3(text):
    p = d.add_paragraph(style="normal")
    r = p.add_run(text)
    set_run_font(r, 11.5, True, font="Times New Roman", color="2E74B5")
    return fmt(p, after=3, before=6, align=WD_ALIGN_PARAGRAPH.LEFT, keep_next=True)


def seq_field(p, kind, number, size):
    """Caption number as a SEQ field (so Word can build a List of Figures/Tables)."""
    fld = OxmlElement("w:fldSimple")
    fld.set(qn("w:instr"), " SEQ %s \\* ARABIC " % kind)
    r = OxmlElement("w:r")
    rpr = OxmlElement("w:rPr")
    rf = OxmlElement("w:rFonts")
    for a in ("w:ascii", "w:hAnsi", "w:cs", "w:eastAsia"):
        rf.set(qn(a), FONT)
    rpr.append(rf)
    b = OxmlElement("w:b"); rpr.append(b)
    sz = OxmlElement("w:sz"); sz.set(qn("w:val"), str(int(size * 2))); rpr.append(sz)
    r.append(rpr)
    t = OxmlElement("w:t"); t.text = str(number); r.append(t)
    fld.append(r)
    p._p.append(fld)


COUNT = {"Figure": 0, "Table": 0}


def caption(kind, text, before=False):
    COUNT[kind] += 1
    p = d.add_paragraph(style="normal")
    r = p.add_run(kind + " ")
    set_run_font(r, SMALL, True, color="1F4E79")
    seq_field(p, kind, COUNT[kind], SMALL)
    r = p.add_run(": ")
    set_run_font(r, SMALL, True, color="1F4E79")
    rich(p, text, SMALL, italic=False)
    fmt(p, after=4 if before else 10, before=2, align=WD_ALIGN_PARAGRAPH.CENTER, keep_next=before)
    return COUNT[kind]


def figure(path, width_cm, text):
    p = d.add_paragraph(style="normal")
    p.add_run().add_picture(path, width=Cm(width_cm))
    fmt(p, after=2, before=6, align=WD_ALIGN_PARAGRAPH.CENTER, keep_next=True)
    return caption("Figure", text)


def next_fig():
    return COUNT["Figure"] + 1


def next_tab():
    return COUNT["Table"] + 1


def cell_shade(cell, fill):
    tcpr = cell._tc.get_or_add_tcPr()
    shd = OxmlElement("w:shd")
    shd.set(qn("w:val"), "clear"); shd.set(qn("w:color"), "auto"); shd.set(qn("w:fill"), fill)
    tcpr.append(shd)


def table(headers, rows, widths_cm, text, size=TBL, first_col_bold=False):
    caption("Table", text, before=True)
    t = d.add_table(rows=1 + len(rows), cols=len(headers))
    t.alignment = WD_TABLE_ALIGNMENT.CENTER
    tblpr = t._tbl.tblPr
    lay = OxmlElement("w:tblLayout"); lay.set(qn("w:type"), "fixed"); tblpr.append(lay)
    borders = OxmlElement("w:tblBorders")
    for edge in ("top", "left", "bottom", "right", "insideH", "insideV"):
        e = OxmlElement("w:" + edge)
        e.set(qn("w:val"), "single"); e.set(qn("w:sz"), "4"); e.set(qn("w:space"), "0"); e.set(qn("w:color"), "000000")
        borders.append(e)
    tblpr.append(borders)
    total = sum(widths_cm)
    widths = [Cm(w * 17.52 / total) for w in widths_cm]
    grid = t._tbl.tblGrid
    for gc, w in zip(grid.findall(qn("w:gridCol")), widths):
        gc.set(qn("w:w"), str(int(w.twips)))
    for ri, row in enumerate([headers] + rows):
        tr = t.rows[ri]
        if ri == 0:
            trpr = tr._tr.get_or_add_trPr()
            th = OxmlElement("w:tblHeader"); th.set(qn("w:val"), "true"); trpr.append(th)
        cant = OxmlElement("w:cantSplit"); cant.set(qn("w:val"), "true"); tr._tr.get_or_add_trPr().append(cant)
        for ci, val in enumerate(row):
            c = tr.cells[ci]
            c.width = widths[ci]
            if ri == 0:
                cell_shade(c, HEAD_FILL)
            p = c.paragraphs[0]
            lines = str(val).split("\n")
            for li, line in enumerate(lines):
                if li:
                    p = c.add_paragraph(style="normal")
                rich(p, line, size, bold=(ri == 0 or (first_col_bold and ci == 0)))
                fmt(p, after=1, line=1.05, align=WD_ALIGN_PARAGRAPH.LEFT)
    sp = d.add_paragraph(style="normal")
    fmt(sp, after=4)
    return t


def code(lines, size=8.5):
    for i, line in enumerate(lines):
        p = d.add_paragraph(style="normal")
        r = p.add_run(line if line else " ")
        set_run_font(r, size, font=MONO, color="1F2933")
        ppr = p._p.get_or_add_pPr()
        shd = OxmlElement("w:shd"); shd.set(qn("w:val"), "clear"); shd.set(qn("w:color"), "auto"); shd.set(qn("w:fill"), "F2F4F7")
        ppr.append(shd)
        p.paragraph_format.left_indent = Cm(0.4)
        fmt(p, after=0, line=1.0, align=WD_ALIGN_PARAGRAPH.LEFT, keep_next=i < len(lines) - 1)
    sp = d.add_paragraph(style="normal"); fmt(sp, after=4)


def field(p, instr, placeholder, size=BODY):
    def fc(kind):
        r = OxmlElement("w:r"); f = OxmlElement("w:fldChar"); f.set(qn("w:fldCharType"), kind); r.append(f); return r
    p._p.append(fc("begin"))
    r = OxmlElement("w:r"); it = OxmlElement("w:instrText"); it.set(qn("xml:space"), "preserve"); it.text = instr; r.append(it); p._p.append(r)
    p._p.append(fc("separate"))
    run = p.add_run(placeholder); set_run_font(run, size)
    p._p.append(fc("end"))


def page_break():
    p = d.add_paragraph(style="normal")
    p.add_run().add_break(WD_BREAK.PAGE)


def clear_runs(p):
    for child in list(p._p):
        if child.tag in (qn("w:r"), qn("w:hyperlink"), qn("w:fldSimple")):
            p._p.remove(child)


# ---------------------------------------------------------------- cover page

def P(i):
    return Paragraph(els[i], d._body)


def T(i):
    return Table(els[i], d._body)


p = P(3); clear_runs(p)
r = p.add_run("Project Title : "); set_run_font(r, 13, True)
r = p.add_run("BeeVM: A Lightweight MicroVM Function-as-a-Service (FaaS) Runtime Engine"); set_run_font(r, 13, False)
p = P(4); clear_runs(p)
r = p.add_run("SDG Aligned & Logo : "); set_run_font(r, 13, True)
r = p.add_run("SDG 9 (Industry, Innovation and Infrastructure), SDG 12 (Responsible Consumption and Production); SDG 13 (Climate Action, indirect)")
set_run_font(r, 12)
p = P(5); clear_runs(p); p.alignment = WD_ALIGN_PARAGRAPH.CENTER
for n in ("09", "12", "13"):
    p.add_run().add_picture(os.path.join(A, "sdg", "sdg-%s.png" % n), width=Cm(2.3))
    p.add_run("   ")

course = T(6)
vals = ["Advanced Cloud Computing (ACC)",
        "ACC  |  Semester VII  |  BE",
        "Richard Joseph",
        "Case Study 06: Lightweight MicroVM FaaS Runtime Engine",
        "17 November 2026"]
for i, v in enumerate(vals, start=1):
    c = course.rows[i].cells[1]
    cp = c.paragraphs[0]; clear_runs(cp)
    r = cp.add_run(v); set_run_font(r, 12)

team = T(9)
members = [("Om Mhatre", "31", "D17C", ""), ("Aditya Sarvankar", "52", "D17C", "")]
for i, m in enumerate(members, start=1):
    for j, v in enumerate(m):
        cp = team.rows[i].cells[j].paragraphs[0]; clear_runs(cp)
        r = cp.add_run(v); set_run_font(r, 12)
for row in list(team.rows)[3:]:
    team._tbl.remove(row._tr)

p = P(11)
text11 = p.text.strip()
p.style = d.styles["normal"]; clear_runs(p)
r = p.add_run(text11); set_run_font(r, 13, True, font="Times New Roman", color="2E74B5")
p = P(12); clear_runs(p)
r = p.add_run("Academic Year: "); set_run_font(r, 13, True)
r = p.add_run("BE"); set_run_font(r, 13)

# footer: project name and page numbers
foot = d.sections[0].footer
fp = foot.paragraphs[0]; clear_runs(fp)
r = fp.add_run("BeeVM: Lightweight MicroVM FaaS Runtime Engine  |  Advanced Cloud Computing  |  Page "); set_run_font(r, 9, color="52514E")
field(fp, " PAGE ", "1", 9)
r = fp.add_run(" of "); set_run_font(r, 9, color="52514E")
field(fp, " NUMPAGES ", "1", 9)

# remove the template's guidance and placeholder pages (keep the cover and its page break)
for el in els[14:]:
    if el.tag != qn("w:sectPr"):
        body.remove(el)

# ---------------------------------------------------------------- contents

def toc_title(text):
    p = d.add_paragraph(style="normal")
    r = p.add_run(text); set_run_font(r, 14, True, font="Times New Roman", color="1F4E79")
    return fmt(p, after=6, before=6, align=WD_ALIGN_PARAGRAPH.LEFT)


toc_title("Table of Contents")
p = d.add_paragraph(style="normal"); field(p, ' TOC \\o "1-2" \\h \\z \\u ', "Right-click and choose Update Field to build the table of contents.")
page_break()
toc_title("List of Figures")
p = d.add_paragraph(style="normal"); field(p, ' TOC \\h \\z \\c "Figure" ', "Update field to list the figures.")
toc_title("List of Tables")
p = d.add_paragraph(style="normal"); field(p, ' TOC \\h \\z \\c "Table" ', "Update field to list the tables.")

# ================================================================= ABSTRACT
h1("Abstract")
para("Serverless computing lets developers upload a single function and pay only while it runs, but the provider "
     "must start an isolated sandbox for that function on demand. Containers start quickly but share the host "
     "kernel, so one tenant's bug or exploit can reach another tenant; full virtual machines isolate well but take "
     "about a second to boot. This \"cold start\" delay and the memory each sandbox costs decide how good and how "
     "cheap a Function-as-a-Service (FaaS) platform can be.")
para("**BeeVM** is a FaaS runtime engine, built for Case Study 06 of the Advanced Cloud Computing course, that runs "
     "every function call inside its own **Firecracker MicroVM** on KVM. A Go control plane (`beevmd`) provides an "
     "authenticated HTTP API, a SQLite function registry, an S3/MinIO artifact store, a warm pool of reusable VMs, "
     "a snapshot manager and a VM manager. Instead of booting, new VMs are **restored from memory snapshots**: a "
     "runtime snapshot (interpreter already loaded) and a function snapshot taken after the user's code was "
     "imported. Functions in static binaries, Python 3.14 and Node.js 24 talk to the host through a guest agent "
     "over vsock. Tenants are separated by the Firecracker jailer (chroot, unique UID, seccomp), cgroups v2 limits, "
     "one network namespace per VM and nftables rules. Prometheus/Grafana and a React dashboard show the system live.")
para("On the development laptop (WSL2 with nested virtualization) a snapshot restore makes a VM ready in "
     "**42 to 50 ms at the median** for all three runtimes, about 20 times faster than a 950 ms boot; the 99th "
     "percentile (57 to 73 ms) is still above the 50 ms target because of nested-virtualization page-fault costs. "
     "Repeat calls on a warm VM take **1 to 2 ms**. All **six tenant-isolation attacks** were contained. One node "
     "held **200 idle VMs at about 3.5 MiB of host memory each**, and 300 fully isolated VMs at 3.9 MiB, with no "
     "failed calls. This density supports SDG 9 and SDG 12 by doing more work per server.")
p = para("**Keywords:** serverless, Function-as-a-Service, Firecracker, MicroVM, KVM, snapshot restore, cold start, "
         "warm pool, multi-tenant isolation, density, Go.", after=4)

# ================================================================= 1 INTRODUCTION
h1("1. Introduction")
h2("1.1 Background and Motivation")
para("Function-as-a-Service (FaaS) platforms such as AWS Lambda, Azure Functions and Google Cloud Functions let a "
     "developer deploy a small piece of code (a function) that runs only when a request or event arrives. The "
     "platform takes care of servers, scaling and billing per call. To make this safe and cheap, the provider must "
     "solve three problems at once, which are also the three grading points of this case study:")
bullet("**Cold-start latency.** When no ready sandbox exists for a function, the first request waits until one is "
       "created. Starting a full virtual machine takes about a second; users notice anything above a few tens of "
       "milliseconds. The case study target is **under 50 ms** using snapshot restore.")
bullet("**Tenant isolation.** Functions of different customers run on the same physical machine. A malicious or "
       "buggy function must not read another tenant's data, attack the host, or slow its neighbours down.")
bullet("**Density.** Every sandbox costs memory and CPU. The more isolated sandboxes one server can hold, the lower "
       "the cost per call and the energy per call.")
para("These goals pull against each other: containers are dense and fast but share one kernel (weaker isolation), "
     "while classic virtual machines isolate well but are slow and heavy. **MicroVMs** are the industry answer: "
     "virtual machines with a minimal device model, a small kernel and a tiny memory footprint. AWS built "
     "Firecracker for exactly this purpose and runs Lambda and Fargate on it [1].")
para("The course concepts applied in this project are virtualization (KVM, hypervisors, paravirtual devices), "
     "multi-tenancy and isolation (namespaces, cgroups, seccomp, network policies), cloud storage (S3 object "
     "storage), elasticity and resource pooling (warm pools, keep-alive, eviction), and observability "
     "(metrics, dashboards, load testing).")

h2("1.2 Problem Context")
para("Commercial platforms are closed: a student cannot see how Lambda decides when to reuse a sandbox, how a "
     "snapshot is restored, or what it costs in memory. Open-source FaaS frameworks (OpenFaaS, Knative, "
     "OpenWhisk) mostly use containers, so they do not show how strong VM isolation can be combined with fast "
     "starts. Research systems [2][3] show that snapshots can cut cold starts, but they are complex research "
     "prototypes. The gap this project fills is a **small, readable and measurable** FaaS engine that uses real "
     "MicroVMs end to end, so that every claim about cold start, isolation and density can be reproduced with "
     "one command on an ordinary laptop.")
para("Case Study 06 of the course document (\"Lightweight MicroVM FaaS Runtime Engine\") asks for exactly this: "
     "Firecracker MicroVMs on a bare-metal Linux node, warm pools and snapshot restore for cold starts under 50 ms, "
     "tenant isolation verified by tests, and a measurement of MicroVM density per node.")

h2("1.3 Intended Users / Stakeholders")
table(["Stakeholder", "Role", "What BeeVM gives them"], [
    ["Function developers (tenants)", "Deploy and call functions in Go/static binaries, Python or Node.js",
     "`beevm` CLI and REST API: deploy, invoke with timing details, logs, list, prewarm; the web dashboard"],
    ["Platform operator", "Runs `beevmd` on a Linux node, manages tenants and keys",
     "`beevmd serve`, tenant/key commands, `doctor` health check, Prometheus metrics and Grafana dashboard"],
    ["Students and faculty", "Learn and evaluate cloud system design",
     "Readable code, a build guide, reproducible benchmarks (`make bench`) and isolation tests"],
    ["Other tenants on the same node", "Indirect stakeholders", "Protection from noisy or malicious neighbours"],
], [3.6, 4.6, 8.0], "Stakeholders of BeeVM", first_col_bold=True)

# ================================================================= 2 PROBLEM
h1("2. Problem Definition, Objectives and Scope")
h2("2.1 Problem Statement")
para("Design and build a multi-tenant Function-as-a-Service runtime that runs **each function invocation inside "
     "its own hardware-isolated Firecracker MicroVM**, brings a new VM to the ready state in **under 50 ms** by "
     "restoring a memory snapshot instead of booting, keeps tenants isolated from each other and from the host "
     "(verified by attack tests), and reports **how many MicroVMs one node can hold** and what each costs in memory. "
     "The system must run on a single Linux node (developed on a Windows laptop under WSL2 with nested KVM), "
     "use only free and open-source software, and expose its behaviour through an API, metrics and benchmarks so "
     "that results can be reproduced.")

h2("2.2 Objectives")
numbered([
    "**Develop** a Go control plane that boots, snapshots, restores and stops Firecracker MicroVMs and talks to a "
    "guest agent over vsock.",
    "**Design and implement** an authenticated HTTP API, a function registry (SQLite) and a CLI to deploy and invoke "
    "functions, with code stored in an S3-compatible artifact store (MinIO).",
    "**Achieve** snapshot-based cold starts with a target of < 50 ms until the VM is ready, and **measure** p50/p90/p99 "
    "against booting.",
    "**Implement** a warm pool (state machine, keep-alive, prewarm, LRU eviction, per-tenant cap) for ~1 ms repeat calls.",
    "**Support** three runtimes (static binary, Python 3.14, Node.js 24) with runtime and per-function snapshots.",
    "**Isolate** tenants with the jailer, cgroups v2, seccomp, a network namespace per VM and nftables, and **verify** "
    "it with six attack functions.",
    "**Instrument** the system with Prometheus metrics, a Grafana dashboard and a live web dashboard.",
    "**Evaluate** cold start, throughput and density with a one-command benchmark (`make bench`) that produces charts "
    "and a report.",
])

h2("2.3 Scope and Deliverables")
h3("In scope")
for t in ["Single-node FaaS engine: API gateway, registry, artifact store client, scheduler/warm pool, snapshot manager, "
          "VM manager, isolation and network, observability.",
          "Runtimes: static Linux binaries, Python 3.14 and Node.js 24 (zip packages with a `handler(event)` entry point).",
          "Developer tools: `beevm` CLI, `make demo`, `beevmd doctor`, project templates (`beevm init`).",
          "Optional, swappable modules: Prometheus + Grafana (`make monitoring`) and the React web dashboard (`make ui`).",
          "Benchmarks (`make bench`, `make bench-jailer`) and isolation tests (`make test-isolation`)."]:
    bullet(t)
h3("Out of scope (for this micro project)")
for t in ["Multi-node scheduling, autoscaling across machines and a global load balancer.",
          "Billing, quotas by money, user sign-up and a web console for operators.",
          "Event sources other than HTTP (queues, cron, storage triggers).",
          "The cloud benchmark run on a bare-metal or non-nested node (planned as milestone 13, not done yet)."]:
    bullet(t)
h3("Deliverables")
table(["Deliverable", "Form", "Status"], [
    ["`beevmd` server and operator tool", "Go binary (serve, tenant, key, snapshot, bench, doctor, boot, invoke)", "Done"],
    ["`beevm` user CLI", "Go binary (login, deploy, invoke, ls, get, rm, prewarm, vms, init)", "Done"],
    ["`beevm-agent` guest agent + runtime shims", "Static Go binary in the rootfs; `shim.py`, `shim.js`", "Done"],
    ["Guest images", "Linux 6.1 kernel, three Alpine 3.24 root filesystems (binary, Python, Node.js)", "Done"],
    ["Monitoring module", "Prometheus 3.15.1 + Grafana 13.3.0 config and dashboard", "Done"],
    ["Web dashboard", "React 19 + Vite + TypeScript + Recharts, served at `/ui/`", "Done"],
    ["Benchmarks and isolation tests", "`make bench`, `make bench-jailer`, `make test-isolation`, k6 scripts, chart script", "Done"],
    ["Documentation", "README, architecture, milestone guide, progress log, developer guide, this report", "Done"],
    ["Cloud run (M13)", "Terraform node, rerun of the benchmarks without nested virtualization", "Not started"],
], [5.0, 8.5, 2.6], "Project deliverables", first_col_bold=True)

h2("2.4 Assumptions and Constraints")
for t in ["**Hardware:** one laptop (Intel Core i7-13620H, 16 logical CPUs visible to WSL, 7.6 GiB RAM for WSL). "
          "KVM is available inside WSL2 through nested virtualization; this adds latency to every guest page fault.",
          "**Software:** Windows 11 host, WSL2 Ubuntu 24.04, Go 1.27.3, Firecracker v1.17.0, no Docker (images are built "
          "with Alpine's static package manager and fakeroot).",
          "**Privileges:** normal operation needs no root. Only the isolated mode (`--isolation jailer`) and its tests "
          "need `sudo` (jailer, cgroups, namespaces, nftables). We decided not to loosen kernel settings such as "
          "unprivileged userfaultfd on the laptop.",
          "**Measurement:** benchmarks must run in the laptop's Performance power mode and plugged in; on battery every "
          "phase is about 2x slower (measured in milestone 6).",
          "**Security and privacy:** API keys are stored only as SHA-256 hashes; no personal data is processed; metrics "
          "contain tenant and function names, so `/metrics` is meant for localhost only.",
          "**Time:** the project was built in milestone-sized sessions between 22 October and 17 November 2026."]:
    bullet(t)

# ================================================================= 3 LITERATURE
h1("3. Background / Literature Review")
para("We studied how production and research systems attack the same three problems. The table summarises each "
     "source and how it shaped BeeVM. Full references are in the References section.")
table(["Source / Year", "Approach or Key Finding", "Strength / Limitation", "Relevance to This Project"], [
    ["[1] Agache et al., \"Firecracker: Lightweight Virtualization for Serverless Applications\", NSDI 2020",
     "Minimal VMM in Rust on Linux KVM with very few emulated devices; one process per MicroVM; runs AWS Lambda and Fargate.",
     "Strength: VM-level isolation with fast boot and small memory overhead. Limitation: a boot still costs far more "
     "than a request, so it is not enough for per-call cold starts.",
     "BeeVM's VMM. We use its REST API, snapshot create/load, vsock and the jailer."],
    ["[2] Ustiugov et al., \"Benchmarking, Analysis, and Optimization of Serverless Function Snapshots\" (vHive/REAP), ASPLOS 2021",
     "Snapshot-restored functions ran 95% slower than memory-resident ones because guest memory is faulted in page by "
     "page; REAP records the working set once and prefetches it, cutting cold-start delay 3.7x on average.",
     "Strength: precise diagnosis of restore cost. Limitation: prefetching needs userfaultfd and privileged setup.",
     "Explains our measured first-call cost (~520-590 page faults at ~37 us each in nested KVM); REAP-style prefetch is our planned next step."],
    ["[3] Du et al., \"Catalyzer: Sub-millisecond Startup for Serverless Computing with Initialization-less Booting\", ASPLOS 2020",
     "Restores a sandbox from a well-formed checkpoint so that application initialisation is skipped on the critical path.",
     "Strength: sub-ms best-case starts. Limitation: deep changes to the sandbox stack.",
     "Motivates our **function snapshot**: the snapshot is taken after the user's code has been imported."],
    ["[4] Oakes et al., \"SOCK: Rapid Task Provisioning with Serverless-Optimized Containers\", USENIX ATC 2018",
     "Container system for serverless with Zygote-style pre-initialised interpreters and a package-aware cache; 18x faster than Docker.",
     "Strength: big gains from pre-imported runtimes. Limitation: containers share the host kernel.",
     "Our **runtime snapshot** (interpreter already started) plays the role of a Zygote, inside a VM."],
    ["[5] Shahrad et al., \"Serverless in the Wild\", USENIX ATC 2020",
     "14-day Azure Functions trace; fixed keep-alive wastes memory and still causes cold starts; proposes an adaptive keep-alive + pre-warm policy.",
     "Strength: real workload data. Limitation: policy needs per-application history.",
     "BeeVM has keep-alive, prewarm and LRU eviction; an adaptive policy is future work."],
    ["[6] Wang et al., \"Peeking Behind the Curtains of Serverless Platforms\", USENIX ATC 2018",
     "Measured AWS Lambda, Azure Functions and Google Cloud Functions: cold starts, placement, and how accounts are isolated (VMs or containers).",
     "Strength: independent measurement of real platforms. Limitation: platforms change over time.",
     "Shows why per-tenant isolation and measured cold starts matter; we test isolation explicitly."],
    ["[7] Manco et al., \"My VM is Lighter (and Safer) than your Container\", SOSP 2017",
     "VMs can be as nimble as containers when the guest is tiny and the toolstack is fast (2.3 ms boot, thousands of guests).",
     "Strength: shows high VM density is possible. Limitation: unikernels / Xen-specific toolstack.",
     "Supports our density goal and our minimal Alpine guest with a shared read-only root filesystem."],
], [3.6, 4.6, 4.2, 4.2], "Literature review summary", size=8.5)

h2("3.1 Identified Gap and Project Contribution")
para("The literature shows the ingredients (MicroVMs, snapshots, pre-initialised runtimes, keep-alive policies) but "
     "either inside closed commercial systems or in research prototypes built on large stacks. BeeVM's contribution "
     "is an **integration**, not a new algorithm:")
for t in ["A complete, small FaaS engine (about 30 Go packages and commands) in which every layer is visible: API, registry, "
          "artifact store, warm pool, snapshot tiers, VM manager, jailer, cgroups, network namespaces and nftables.",
          "**Two snapshot tiers** (runtime and per-function) for three runtimes, built automatically in the background after "
          "each deploy, with correct invalidation when images, code or build procedure change.",
          "**Measured, reproducible claims:** one command reproduces cold-start percentiles, throughput, density and the "
          "isolation table on any KVM-capable Linux machine, and reports honestly when a target is missed.",
          "Practical fixes found along the way: clock and random-number reseeding of cloned VMs after restore, a settle "
          "time before snapshotting (without it every restored VM answered ~850 ms late), and a freeze-then-kill sweep "
          "for fork bombs."]:
    bullet(t)

# ================================================================= 4 DESIGN
h1("4. Proposed Solution and System Design")
h2("4.1 Proposed Solution")
para("BeeVM runs one daemon, `beevmd`, per Linux node. Tenants use the `beevm` CLI (or plain HTTP) with an API key. "
     "A typical workflow is:")
numbered(["`beevm login` saves the server address and the tenant's key (file mode 0600).",
          "`beevm init python my-fn` creates a function template; `beevm deploy my-fn ./my-fn` zips and uploads it. "
          "The server checks the package, stores the code (local cache, and S3/MinIO when configured), records the "
          "function in SQLite and builds a **function snapshot** in the background.",
          "`beevm invoke my-fn --payload '{...}' -v` sends the request. The warm pool gives the call an idle VM of the "
          "same function if one exists (**warm**, ~1-2 ms); otherwise a new VM is **restored** from the function snapshot "
          "(~45 ms until ready) or, if that does not exist yet, from the runtime snapshot plus upload and import; booting "
          "(~950 ms) is only a fallback.",
          "The guest agent runs the handler as an unprivileged user and returns the JSON result; timing headers tell the "
          "caller how the VM started and where the time went. The VM stays idle for the keep-alive period for the next call.",
          "Operators watch the node in Grafana or the web dashboard and run `make bench` to measure it."])

h2("4.2 System Architecture / Block Diagram / Conceptual Design")
fa = figure(ARCH, 16.5, "BeeVM architecture (planning diagram, October 2026). As built, the runtimes are Python 3.14 and "
            "Node.js 24 (Alpine 3.24 ships no 3.12/20), the CLI uses Go's standard flag package, and Prometheus/Grafana run "
            "from pinned binaries without Docker.")
para("Figure %d shows the components. Clients and the two optional dashboards (dashed) talk only to the public API. "
     "Inside `beevmd`, the **API Gateway & HTTP Router** authenticates the tenant and looks the function up in the "
     "**Function Registry**; the **Scheduler & Warm Pool** decides whether a call can reuse a VM; the **Snapshot "
     "Manager** builds and finds snapshots; the **VM Manager** drives Firecracker; **Isolation & Network** wraps each "
     "VM in a jail, a cgroup and a network namespace; **Observability** exports metrics and logs. Below the daemon run "
     "N MicroVMs, each with its own kernel and the guest agent. Snapshots and images live on local disk; function code "
     "is backed up in an S3-compatible artifact store." % fa)
ff = figure(os.path.join(A, "diag-flow.png"), 16.0, "Request flow of one call and the three ways a VM can be obtained")
para("Figure %d follows a single request. The pool key is (tenant, function, code hash, memory, vCPUs, runtime, handler), "
     "so a VM is **never shared between functions or tenants**, not even when the code is identical. After the call the VM "
     "returns to the pool unless the call timed out, the agent connection broke, or the host killed the VM." % ff)
fs = figure(os.path.join(A, "diag-states.png"), 16.0, "Life cycle of a VM in the warm pool (allowed state changes)")
para("Figure %d is the warm-pool state machine. Every state change goes through one table of allowed moves; any other "
     "move is a programming error and is rejected. `--max-vms` counts VMs in every state except Destroyed, because idle "
     "VMs use memory too." % fs)

h2("4.3 Module Description")
table(["Module / Component", "Responsibility", "Input", "Output"], [
    ["API Gateway & HTTP Router (`internal/api`)", "REST API, tenant key check, invoke handling, timing headers, SSE event stream, `/metrics`, `/healthz`",
     "HTTP requests with `Authorization: Bearer <key>`", "JSON answers, events, metrics"],
    ["Function Registry (`internal/registry`)", "Tenants, hashed API keys, functions (runtime, handler, memory, vCPUs, timeout, code hash); schema migrations",
     "Deploy/delete/list calls", "Function records, key verification"],
    ["Artifact store + code cache (`internal/store`, `internal/codestore`)", "Content-addressed code storage: local cache `functions/<sha256>`, S3/MinIO as source of truth when configured",
     "Code bytes, SHA-256", "Cached file path, cache hit/miss"],
    ["Package rules (`internal/pkg`)", "Runtime detection, zip safety (no absolute paths, `..`, symlinks; max 2,000 files, 32 MiB)",
     "Zip package or binary", "Accepted package or error"],
    ["Scheduler & Warm Pool (`internal/pool`, `internal/runner`)", "Acquire/Release, keep-alive reaper, prewarm, drain on redeploy, LRU eviction, per-tenant cap; cold/snapshot/warm runners",
     "Call request (pool key)", "A ready VM, the result, start kind"],
    ["Snapshot Manager (`internal/snapshot`)", "Build, find and invalidate runtime snapshots and function snapshots; keys hash every input",
     "Runtime, images, function code, build version", "`snapshots/<key>/` (vmstate, mem, meta.json)"],
    ["VM Manager (`internal/vmm`)", "Firecracker boot, restore, pause, snapshot, stop; own API client; vsock client to the agent; jailer integration",
     "VM config, snapshot", "Running VM handle, timings"],
    ["Isolation & Network (`internal/isolation`, `internal/netns`)", "Slots (UIDs), cgroups v2, chown-back, network namespaces, TAP/veth, nftables rules, stale cleanup",
     "Tenant, slot", "Jailed, limited, networked VM"],
    ["Guest agent (`cmd/beevm-agent`, `internal/agent`, `guest/shims`)", "Inside the VM: /resume (clock+entropy), /package, /load, /invoke; runs code as user `fn`; Python/Node shims",
     "Requests over vsock", "Function output, logs, exit code"],
    ["Observability (`internal/metrics`, `internal/events`)", "Prometheus histograms and gauges (latency, VM states, PSS/RSS), pub/sub hub for the live stream",
     "Pool and request hooks", "`/metrics`, `/api/v1/events`"],
    ["CLI (`cmd/beevm`)", "login, deploy, invoke (-v, -l), ls, get, rm, prewarm, vms, whoami, init", "User commands", "API calls, readable output"],
    ["Web dashboard (`web/`, optional)", "Live VM grid, invoke panel, cold vs warm chart, density tiles, warm pools", "Public API + SSE", "Browser UI at `/ui/`"],
    ["Monitoring (`deploy/monitoring`, optional)", "Prometheus scrape config, Grafana dashboard \"BeeVM\"", "`/metrics` every 1 s", "Graphs and stat panels"],
], [4.0, 6.0, 3.2, 3.3], "Modules of BeeVM", size=8.5, first_col_bold=True)

h2("4.4 Data / Inputs / Dataset")
para("BeeVM does not use a dataset. Its inputs are function packages, invocation payloads and configuration. The test "
     "inputs used throughout this report are:")
table(["Input", "Format", "Used for"], [
    ["`hello-go-binary`", "Static Linux binary (Go, 2.5 MB), reads JSON on stdin", "Binary runtime, cold start, density"],
    ["`hello-python`", "Zip with `handler.py` (523 B), `handler(event)`", "Python runtime and snapshots"],
    ["`hello-node`", "Zip with `index.js` (548 B), async handler", "Node.js runtime and snapshots"],
    ["`matrix`", "Static binary: 8x8 matrix product", "A function that does real computation"],
    ["Attack functions: `forkbomb`, `memhog`, `cpuhog`, `netprobe`, `hostread`", "Static binaries", "Isolation tests (Section 6)"],
    ["Payloads", "JSON objects, e.g. `{\"name\":\"Om\"}`", "Every call"],
    ["Server configuration", "Flags: `--runner`, `--keep-alive`, `--max-vms`, `--max-vms-per-tenant`, `--isolation`, `--cpu-limit`, `--egress`", "Experiments"],
], [4.6, 6.0, 5.4], "Inputs and test functions", first_col_bold=True)
para("Stored data: `beevm.db` (SQLite registry; keys only as SHA-256 hashes), `functions/<sha256>` (code cache), "
     "`snapshots/<key>/` (one memory file of the VM's size, e.g. 128 MiB, plus device state). No personal data is collected.")

h2("4.5 Algorithm / Design Decisions")
table(["Decision", "Choice", "Reason"], [
    ["Control plane language", "Go 1.27 with `firecracker-go-sdk`, `go-chi/chi`, pure-Go SQLite (`modernc.org/sqlite`)", "Good concurrency primitives for the pool; single static binary; no C toolchain"],
    ["Host-guest channel", "vsock (Firecracker hybrid vsock over a Unix socket), not the network", "Works without guest networking; one socket per VM folder, so restored clones get their own socket"],
    ["Network", "One network namespace per VM", "Every restored clone keeps the same guest IP (172.16.0.2) and is NATed to a unique address"],
    ["Snapshot storage", "Local disk (NVMe); S3/MinIO only for function code and backup", "Restore maps the memory file lazily; network storage would add latency"],
    ["Root filesystem", "One shared read-only image per runtime; `/run` and `/tmp` are RAM in the guest", "No copy per VM (saved ~0.4 s per cold call) and shared page cache"],
    ["Snapshot tiers", "Runtime snapshot (interpreter started) and function snapshot (code imported)", "Skips boot and import on the critical path [3][4]"],
    ["Snapshot keys", "Hash of Firecracker version, kernel and rootfs hashes, boot args, memory, vCPUs, runtime, isolation, build version", "Old snapshots become stale automatically when anything changes"],
    ["Pool policy", "Per-function pools, keep-alive TTL (default 60 s), prewarm on request, LRU eviction across functions, optional per-tenant cap", "Simple, predictable; adaptive policies [5] left for later"],
    ["Isolation", "Jailer + cgroups v2 + seccomp + netns + nftables; guest user `fn` with process limits", "Defence in depth; each layer covers a different attack"],
    ["Optional modules", "Dashboard and Prometheus/Grafana use only the public API and `/metrics`", "Can be deleted without touching core code (tested)"],
], [3.3, 6.6, 6.6], "Key design decisions", size=8.5, first_col_bold=True)
h3("Pseudocode: handling one invocation")
code(["function Invoke(tenant, fnName, payload):",
      "    f = registry.Lookup(tenant, fnName)            # 404 if missing",
      "    key = PoolKey(tenant, f.id, f.codeSHA, f.memory, f.vcpus, f.runtime, f.handler)",
      "    vm, start = pool.Acquire(key, create = NewVM)  # warm if an idle VM of this key exists",
      "    result = vm.agent.Invoke(payload, timeout = f.timeout)",
      "    healthy = not (timeout or agentBroken or vmProcessExited)",
      "    pool.Release(vm, healthy)                      # healthy -> Idle, else Recycling",
      "    return result with headers X-BeeVM-Start, -Snapshot, -Boot-Ms, -Upload-Ms, -Load-Ms, -Function-Ms",
      "",
      "function NewVM(key):",
      "    if snap = snapshots.LookupFunction(key):      # code already imported",
      "        vm = vmm.Restore(snap); vm.agent.Resume(hostClock, randomBytes); return vm, 'function'",
      "    if snap = snapshots.EnsureRuntime(key.runtime):",
      "        vm = vmm.Restore(snap); vm.agent.Resume(...); vm.agent.Upload(code); vm.agent.Load(handler)",
      "        snapshots.PrepareFunctionInBackground(key)  # next VM will be faster",
      "        return vm, 'runtime'",
      "    return vmm.Boot(kernel, rootfs[key.runtime]), 'boot'   # fallback"])
h3("Pseudocode: building a function snapshot")
code(["function BuildFunctionSnapshot(fn):",
      "    vm = vmm.Restore(runtimeSnapshot(fn.runtime))",
      "    vm.agent.Resume(); vm.agent.Upload(fn.code); vm.agent.Load(fn.handler)   # import once",
      "    sleep(200 ms)                 # settle: let the guest finish background work",
      "    vm.Pause(); vm.CreateSnapshot(tmpDir)                                     # vmstate + mem",
      "    rename(tmpDir, 'snapshots/fn-' + hash(parentKey, tenant, fn, code, handler, buildVersion))"])

# ================================================================= 5 IMPLEMENTATION
h1("5. Methodology and Implementation")
h2("5.1 Workflow / Development Method")
para("We worked in **13 session-sized milestones**, each leaving a working, demonstrable system. Every session followed "
     "the same loop: read the design and progress log, plan, build, test (unit tests, then tests that boot real VMs), "
     "update the progress log, commit. A finished milestone could later be **refined** in a short session without "
     "changing its public interfaces. The must-do order covered all grading points first "
     "(M1 → M2 → M3 → M4 → M6 → M7 → M9 → M12); M5, M8, M10 and M11 were added in between.")
para("The timeline below is taken from the Git history (commit times in IST); Appendix A shows the raw `git log` output.")
table(["Date and time (IST)", "Commit", "Work completed", "Owner"], [
    ["22 Oct 2026, 22:50", "2a0cd3d", "Project setup: architecture plan, diagrams, repository, coding rules", "Om"],
    ["22 Oct 2026, 23:16", "40c3900", "13-milestone session guide", "Om"],
    ["23 Oct 2026, 00:00", "77d22dd", "**M1** Environment, setup script, kernel + Alpine rootfs, hand-booted MicroVM", "Om"],
    ["23 Oct 2026, 00:48", "8c5b82b", "**M2** Boot and stop a VM from Go (`internal/vmm`, `beevmd boot`)", "Om"],
    ["23 Oct 2026, 02:27", "0337349", "Step-by-step build guide", "Om"],
    ["24 Oct 2026, 14:52", "7d9796c", "**M3** Guest agent over vsock; first function runs", "Om"],
    ["26 Oct 2026, 05:46", "840f19c", "**M4** HTTP API, SQLite registry, hashed keys, CLI", "Om"],
    ["26 Oct 2026, 14:07", "92e55be", "**M5** Artifact store with MinIO/S3, content-addressed cache", "Aditya"],
    ["27 Oct 2026, 02:35", "d3d5406", "Demonstration script for milestones 1-5", "Om"],
    ["27 Oct 2026, 05:13", "c2571a4", "**M6** Snapshots and fast restore; first cold-start benchmark", "Om"],
    ["03 Nov 2026, 02:10", "c60df43", "**M6 refine** Faster, steadier restore (agent phase, settle, power-mode finding)", "Om"],
    ["03 Nov 2026, 04:20", "64cb026", "**M7** Warm pool (state machine, keep-alive, prewarm, LRU)", "Om"],
    ["06 Nov 2026, 03:58", "fdb7fc3", "**M8** Python and Node.js runtimes, function snapshots", "Om"],
    ["06 Nov 2026, 12:06", "d65f375", "**M9** Isolation and networking; 6 attack tests", "Om"],
    ["06 Nov 2026, 13:01", "7d11342", "**M10** Observability: metrics, Prometheus + Grafana, load generator", "Aditya (Prometheus, Grafana); Om (core metrics hooks)"],
    ["06 Nov 2026, 14:29", "1037e5c", "**M11** Web dashboard; SSE event stream", "Aditya (dashboard); Om (core API)"],
    ["07 Nov 2026, 14:28", "9bc9b9b", "Build guide, learning notes and demo updated through M11", "Om"],
    ["16 Nov 2026, 13:53", "ef3b241", "**M11 refine** Function filter, live function list, tenant name, smaller bundle", "Aditya"],
    ["17 Nov 2026, 13:34", "fd856fe", "Usability: README, function logs, `make demo`, `doctor`, `login`, tenant cap, 502 on host kill", "Om"],
    ["17 Nov 2026, afternoon", "(this report)", "**M12** Benchmarks and density: `beevmd bench density`, k6, charts, `make bench`; isolation and jailer runs; report", "Om"],
], [3.3, 1.7, 8.6, 3.0], "Work log with timestamps (from the Git history)", size=8.5)
para("In total the repository has 31 commits over 27 days. The milestone 12 work and this report were completed on "
     "17 November 2026; the screenshots in Section 5.4 were captured the same day between 09:24 and 09:32 UTC "
     "(14:54-15:02 IST), as their timestamps show.")

h2("5.2 Technologies, Tools and Resources")
table(["Category", "Technology / Tool", "Purpose / Justification"], [
    ["Programming language / framework", "Go 1.27.3; `firecracker-go-sdk` (pinned commit); `go-chi/chi`; `prometheus/client_golang` v1.25.0; `aws-sdk-go-v2`; `google/nftables` v0.3.0; `vishvananda/netlink`",
     "Control plane, CLI, guest agent (static binary), tests"],
    ["Virtualization", "Firecracker v1.17.0 and its jailer; Linux KVM; guest kernel `vmlinux` 6.1", "MicroVMs with hardware isolation and snapshot support"],
    ["Guest OS and runtimes", "Alpine Linux 3.24 (built with `apk.static` + fakeroot), Python 3.14, Node.js 24", "Small read-only root filesystems per runtime"],
    ["Database / storage", "SQLite (`modernc.org/sqlite`, pure Go); MinIO (S3 API) built from pinned source", "Registry; artifact store for function code"],
    ["Observability", "Prometheus 3.15.1, Grafana 13.3.0 (pinned tarballs, no Docker)", "Metrics and dashboards"],
    ["Web dashboard", "React 19.3.1, Vite 8.3.4, TypeScript 6.0.3, Recharts 3.10.1, Vitest 5.0.5; Node.js 24.22.2 for the build", "Live demo screen"],
    ["Testing and benchmarking", "Go `testing` (+ `-race`), build tag `kvm` for VM tests; k6 2.4.0; Python 3.12 + matplotlib 3.6.3; `beevmd bench`", "Unit, integration, isolation tests; load and density benchmarks; charts"],
    ["Development tools", "WSL2 Ubuntu 24.04, GNU Make, Git, VS Code", "Build and version control"],
    ["Hardware", "Laptop: Intel Core i7-13620H (16 threads in WSL), 7.6 GiB RAM in WSL, NVMe SSD; Performance power mode", "KVM host for development and all measurements"],
], [3.6, 6.8, 6.0], "Technologies, tools and resources", size=8.5, first_col_bold=True)

h2("5.3 Implementation Details")
para("This section describes each milestone: what was built, the problems met and how they were solved.")
h3("M1-M2: Environment and VM control from Go")
para("`scripts/setup-wsl.sh` (re-runnable) adds the user to the `kvm` group and installs Go, Firecracker, the guest kernel "
     "and the root filesystem. `internal/vmm` wraps the Firecracker SDK: create, start, stop and kill a VM, capture its "
     "console in a log file, and give each VM its own working folder that is removed afterwards. A boot took ~1.8 s at "
     "first; later we found the laptop's power mode made everything ~2x slower, and in Performance mode a boot takes "
     "~0.95 s.")
h3("M3: Guest agent over vsock")
para("`beevm-agent` is a static Go binary baked into the rootfs and started at boot. It listens on vsock and serves "
     "`/health`, `/invoke`, and later `/resume`, `/package` and `/load`. The host connects through Firecracker's "
     "Unix-socket bridge. For the binary runtime the agent writes the uploaded file, runs it with the payload on stdin and "
     "returns stdout as JSON together with the exit code and the function's log.")
h3("M4-M5: API, registry, CLI and artifact store")
para("`internal/api` (chi router) serves `PUT /api/v1/functions/{name}` (multipart upload), `POST /invoke/{name}`, list, "
     "get, delete, prewarm and VM listing. Keys are random, shown once and stored as SHA-256 hashes. The CLI `beevm` "
     "wraps the API. In M5 the code moved to an S3-compatible store: deploy uploads to MinIO, the node keeps a "
     "content-addressed cache `functions/<sha256>`, so a call downloads code only on the first use (shown as "
     "`code cache: hit/miss` in `beevm invoke -v`). Switching to AWS S3 is a configuration change only.")
h3("M6: Snapshots and fast restore (the core)")
para("The Snapshot Manager boots a VM, waits for the agent, lets it **settle for 1 s**, pauses it and writes the memory "
     "file and device state. Restore starts a new Firecracker process inside a fresh folder and calls "
     "`PUT /snapshot/load` with `resume_vm`; guest memory is mapped from the file and loaded lazily on first touch, so "
     "five parallel restores share one file copy-on-write. Several problems had to be solved:")
for t in ["**Clones share state.** Every restored VM starts with the clock and random-number state of the snapshot. The agent's "
          "`/resume` call sets the guest clock from the host and adds 64 fresh random bytes to the kernel's entropy pool "
          "(Firecracker's VMGenID also triggers a reseed; we keep both).",
          "**Settle before snapshotting.** A snapshot taken right after the agent started made the first reply after every "
          "restore take ~850 ms; letting the VM run 0.5-1 s first brought it to 5-15 ms.",
          "**Fast readiness check.** The SDK polls every 10 ms, so we wrote a small Firecracker API client that polls every "
          "1 ms and waits for the agent with one `/resume` request.",
          "**Where the time goes** (refine): ~1.5 ms process spawn, ~23 ms snapshot load (inside Firecracker, varies with "
          "host scheduling noise), ~21 ms until the agent answers, mostly ~520-590 guest page faults at ~37 us each under "
          "nested virtualization."]:
    bullet(t)
h3("M7: Warm pool")
para("`internal/pool` has no Firecracker code and is tested with fake VMs and a fake clock (25 tests covering every state "
     "transition). `Acquire` takes the newest idle VM of the key, else creates one if a slot is free, else evicts the least "
     "recently used idle VM of any function, else waits. Waiters sleep on a broadcast channel; the lock is never held "
     "while a VM is created or destroyed. A reaper recycles VMs idle longer than the keep-alive. Redeploy and delete "
     "drain the function's VMs. Result: repeat calls in **1.6 ms p50** instead of ~88 ms for restore plus call.")
h3("M8: Python and Node.js runtimes, function snapshots")
para("Runtime images are built without Docker: Alpine's static package manager installs `python3`/`nodejs` into the "
     "unpacked mini root filesystem. A long-lived **shim** per runtime (`shim.py`, `shim.js`) imports the user's module once "
     "and calls `handler(event)` for every request, using file descriptors 3 and 4 for the protocol so that user prints "
     "cannot break it. After a deploy, a **function snapshot** is built in the background (~0.45-0.8 s): restore the "
     "runtime snapshot, upload, import, settle 200 ms, snapshot. Measured effect: cold start p50 **58 ms (Python), 85 ms "
     "(Node.js), 73 ms (binary)** until the first response, 33% / 43% / 16% better than runtime snapshot + import.")
h3("M9: Isolation and networking")
para("With `--isolation jailer` every VM runs through the Firecracker jailer in a chroot that contains only hard links to "
     "its kernel, rootfs and snapshot, as UID 900000+slot with a seccomp filter and no capabilities, inside a cgroup "
     "`beevm/t<tenant>/<vm>` with `memory.max` (guest + 64 MiB), `pids.max` 32 and `cpu.max`. Each slot has a network "
     "namespace with `tap0` and a veth pair; the guest always sees 172.16.0.2 and is NATed to a unique clone address. A "
     "host nftables table allows traffic only between VMs of the same tenant and drops VM-to-host traffic; internet "
     "access is off unless `--egress nat`. Inside the guest the function runs as user `fn` with a process limit and the "
     "agent freezes and kills leftover processes after each call (Figure %d)." % (next_fig()))
figure(os.path.join(A, "diag-layers.png"), 16.0, "Isolation layers around one function (outermost: host firewall; innermost: the function process)")
h3("M10-M11: Observability and web dashboard (Aditya)")
para("`internal/metrics` exports counters and histograms for calls, latency by start kind, VM-ready time by snapshot tier, "
     "upload/load/function times, recycles and snapshot builds, and reads VM states and **PSS/RSS memory** of every "
     "Firecracker process at scrape time. Prometheus scrapes `/metrics` every second and Grafana shows the \"BeeVM\" "
     "dashboard. The React dashboard uses only the public API and a Server-Sent Events stream (`/api/v1/events`) to show "
     "VMs changing state live; deleting `web/` or `deploy/monitoring/` leaves the core building and passing all tests.")
h3("Usability work and M12: benchmarks and density")
para("`make demo` brings a fresh machine to a running server with three ready functions; `beevmd doctor` checks KVM, "
     "images, snapshots, server, leftover VMs and the power mode and prints the fix for each problem. If the host kills "
     "a VM during a call (memory limit), the API now answers 502 with an explanation instead of 500. Milestone 12 added "
     "`beevmd bench density` (prewarm VMs in steps through the public API, read node memory, probe warm latency), k6 "
     "scripts for throughput, a chart and report script, and `make bench`, which runs everything in about 5 minutes "
     "(`BENCH_QUICK=1`).")
h3("API summary")
table(["Method and path", "Purpose"], [
    ["`PUT /api/v1/functions/{name}`", "Deploy or update a function (multipart: config + code)"],
    ["`GET /api/v1/functions`, `GET/DELETE /api/v1/functions/{name}`", "List, show, delete functions"],
    ["`POST /invoke/{name}`", "Call a function; optional `X-BeeVM-Log-Type: tail` returns the last 4 KB of its log"],
    ["`POST /api/v1/functions/{name}/prewarm?count=N`", "Start N idle VMs for a function"],
    ["`GET /api/v1/vms`, `GET /api/v1/node`, `GET /api/v1/me`", "The tenant's VMs; node density and memory; who am I"],
    ["`GET /api/v1/events`", "Server-Sent Events: VM state changes, invocations, deploys (own tenant only)"],
    ["`GET /metrics`, `GET /healthz`", "Prometheus metrics (keep on localhost); health check"],
], [7.0, 9.5], "Public HTTP API (all `/api` and `/invoke` paths need `Authorization: Bearer <key>`)", size=8.5)

h2("5.4 User Interface / Prototype / Output")
para("All screenshots below were taken from the running system on 17 November 2026. Terminal outputs are the exact "
     "text the commands printed (API keys masked); the grey timestamp before each command is the capture time on the "
     "WSL clock (UTC; IST = UTC + 5:30).")
f1 = figure(os.path.join(A, "term", "t01-doctor.png"), 12.5, "`beevmd doctor`: every part of the setup checked before starting")
f2 = figure(os.path.join(A, "term", "t02-deploy.png"), 15.0, "Deploying one function per runtime with the `beevm` CLI and listing them")
f3 = figure(os.path.join(A, "term", "t03-invoke.png"), 16.5, "Invoking functions: restore from snapshots on the first call, then a warm call in 2 ms (server time)")
para("Figure %d shows the three start kinds in practice. The first Python call restored a VM from the runtime snapshot "
     "(161 ms including the resume, then upload 15 ms and import 92 ms), the second call reused the same VM (`ab75edfc`) "
     "and took **2 ms** inside the server. The Go binary was restored from its **function snapshot**: no upload and no "
     "import. The function snapshots for Python and Node.js were still being built when these first calls arrived; later "
     "calls used them." % f3)
f4 = figure(os.path.join(A, "term", "t04-pool.png"), 12.5, "Prewarming three VMs (61 ms in total) and listing the tenant's VMs in the warm pool")
f5 = figure(os.path.join(A, "term", "t07-load.png"), 16.5, "`beevmd bench load`: 400 calls at 20 calls/s, 399 warm, all successful")
fdl = figure(os.path.join(A, "shot-dashboard-login.png"), 8.5, "Web dashboard: a tenant opens it with an API key (kept in session storage)")
fd1 = figure(os.path.join(A, "shot-dashboard-top.png"), 16.5, "Web dashboard during a load run: density tiles, live MicroVM grid (8 idle VMs after prewarming), invoke panel with timings")
fd2 = figure(os.path.join(A, "shot-dashboard-full.png"), 16.5, "Web dashboard: cold vs warm latency chart (log scale, 50 ms line), per-start statistics and warm pools")
para("In Figure %d the dots form two groups: warm calls between 1 and 10 ms and a few restores (blue) above 100 ms. "
     "The table under the chart gives warm p50 3.8 ms and restore VM-ready p50 82 ms. That restore number is higher "
     "than the 45 ms of the benchmark because here restores happened **under load, several at the same time** at the "
     "start of each burst; the benchmark measures one restore at a time." % fd2)
fg1 = figure(os.path.join(A, "shot-grafana-top.png"), 16.5, "Grafana dashboard \"BeeVM\": calls per second by start kind, warm hit ratio 99.2%, latency p50/p99 per start kind, VM-ready time per snapshot tier")
fg2 = figure(os.path.join(A, "shot-grafana-bottom.png"), 16.5, "Grafana: VMs by state against the maximum, memory per VM (PSS vs RSS), node memory, VMs recycled by reason (keep-alive), calls per function")
fp1 = figure(os.path.join(A, "shot-prometheus-targets.png"), 15.0, "Prometheus target health: `beevmd` scraped every second, state UP")
fp2 = figure(os.path.join(A, "shot-prometheus-graph.png"), 16.0, "Prometheus query: median call latency by start kind (restore vs warm) from the histogram")
f6 = figure(os.path.join(A, "term", "t06-api.png"), 16.5, "Raw API outputs: node density from `/api/v1/node` and Prometheus series from `/metrics`")

h2("5.5 Team Contribution")
table(["Team Member", "Assigned Responsibility", "Work Completed / Evidence"], [
    ["Om Mhatre (Roll No. 31, D17C)", "Core engine, isolation, benchmarks, integration and documentation",
     "M1-M4 environment, VM manager, guest agent, API/registry/CLI; M6 snapshots and restore tuning; M7 warm pool; "
     "M8 Python/Node.js runtimes and function snapshots; M9 jailer, cgroups, network namespaces, nftables and the six attack "
     "tests; core API for metrics and events; usability work (`make demo`, `doctor`, `login`, function logs, tenant cap); "
     "M12 benchmarks and density (`bench density`, k6 scripts, `plot_results.py`, `make bench`); README, guide, report. "
     "Evidence: commits listed in Table 7, Figures 5-9 and 18-22."],
    ["Aditya Sarvankar (Roll No. 52, D17C)", "Artifact store, monitoring stack and web dashboard",
     "M5 MinIO/S3 artifact store and content-addressed code cache (`internal/store`, `scripts/minio.sh`); M10 Prometheus "
     "and Grafana module (`deploy/monitoring`, `scripts/monitoring.sh`, dashboard \"BeeVM\"); M11 React web dashboard "
     "(`web/`) and its refinement (function filter, live function list, smaller bundle). Evidence: commits 92e55be, "
     "7d11342, 1037e5c, ef3b241; Figures 10-17."],
], [3.6, 4.0, 8.9], "Team contribution", size=8.5, first_col_bold=True)

# ================================================================= 6 TESTING
h1("6. Testing, Evaluation and Results")
h2("6.1 Test Plan and Test Cases")
para("Testing ran on four levels: **unit tests** (no KVM; 305 tests, run with the race detector for the concurrent "
     "packages), **integration tests** that boot real MicroVMs (build tag `kvm`; 34 test functions in 13 files, 316 "
     "passing test cases at milestone 11), **isolation tests** as root (`make test-isolation`), and **benchmarks** "
     "(`make bench`, `make bench-jailer`). Environment: the laptop described in Section 2.4, WSL2, Performance mode.")
table(["Test ID", "Test Input / Scenario", "Expected Result", "Observed Result", "Status"], [
    ["TC-01", "Boot a MicroVM from Go and stop it (`beevmd boot`, KVM test)", "Console shows Linux boot; no process or folder left", "Boot ~0.95 s; clean-up verified", "Pass"],
    ["TC-02", "Deploy binary, Python and Node.js functions", "Created with detected runtime and handler", "Figure 6: created, `beevm ls` lists all", "Pass"],
    ["TC-03", "Invoke with a valid key", "HTTP 200 and the function's JSON", "Figure 7: correct greeting per runtime", "Pass"],
    ["TC-04", "Invoke with a missing or wrong key", "HTTP 401, no VM started", "401 in API unit tests; CLI exits with code 2", "Pass"],
    ["TC-05", "Upload a zip with `../` path, absolute path or symlink (invalid input)", "Rejected with 400 before storing", "Rejected by `internal/pkg` rules (unit tests)", "Pass"],
    ["TC-06", "Python function with a syntax error", "502 \"function failed to load\"", "502 with the import error (KVM end-to-end test)", "Pass"],
    ["TC-07", "Function exceeds its timeout", "Call fails; VM thrown away, not reused", "VM replaced (KVM test)", "Pass"],
    ["TC-08", "Second call of the same function", "Served warm by the same VM", "Figure 7: same VM, 2 ms server time", "Pass"],
    ["TC-09", "VM idle longer than keep-alive", "VM recycled, no process or folder left", "Reaped after keep-alive (KVM test; Figure 14 keep_alive recycles)", "Pass"],
    ["TC-10", "Redeploy or delete a function", "Old VMs drained, old snapshots removed", "Verified in KVM tests", "Pass"],
    ["TC-11", "Prewarm 3 VMs", "3 idle VMs ready", "Figure 8: started 3 of 3 in 61 ms", "Pass"],
    ["TC-12", "Node full (`--max-vms`) / tenant at its cap (boundary)", "LRU idle VM evicted, else 503; a tenant evicts only its own VMs", "Pool unit tests (25 + 4 tenant-cap tests)", "Pass"],
    ["TC-13", "Two restored clones read the clock and random bytes", "Clock correct within 1 s; different random bytes", "Verified (agent resume tests)", "Pass"],
    ["TC-14", "Host file read attack", "Canary not found; tricks denied", "Table 17: 0 hits in 85 files; 4/4 denied", "Pass"],
    ["TC-15", "Cross-tenant network probe", "Other tenant and host unreachable", "Table 17: blocked both ways; scan finds only own VM", "Pass"],
    ["TC-16", "Fork bomb", "Limited; VM, agent and neighbour fine", "Table 17: 3,296 forks refused; neighbour 2.9 -> 2.2 ms", "Pass"],
    ["TC-17", "Memory hog (1 GiB in a 128 MiB VM)", "Function killed in the guest; VM reusable", "Table 17: killed; same VM served the next call", "Pass"],
    ["TC-18", "CPU hog with `--cpu-limit 50`", "Held to ~0.5 CPU", "Table 17: 0.50 CPU, throttled 43 times", "Pass"],
    ["TC-19", "Host memory limit below the guest size (failure case)", "Host kills that VM; 502; neighbour fine", "Table 17: 502, VM removed, neighbour ok", "Pass"],
    ["TC-20", "Deploy with MinIO configured; invoke twice", "Code uploaded to the bucket; first call cache miss, then hit", "Verified with `make test-s3` and `invoke -v`", "Pass"],
    ["TC-21", "Live event stream during calls", "States restoring, busy, idle, recycling, destroyed in order", "`TestE2EEvents` and Figures 11-12", "Pass"],
    ["TC-22", "Remove the optional dashboard (`web/`)", "Core builds and all tests pass", "Verified in M11", "Pass"],
    ["TC-23", "Benchmark tools (`make test-bench`)", "Chart script tests pass; k6 scripts valid", "9 Python tests OK; `k6 inspect` OK (Appendix A)", "Pass"],
], [1.3, 4.6, 3.9, 4.6, 1.3], "Test cases and results", size=8)

h2("6.2 Evaluation Metrics")
table(["Metric", "Measurement Method / Formula", "Observed Value", "Interpretation"], [
    ["VM-ready time (cold start)", "Time from starting Firecracker until the guest agent answered `/resume`; p50/p99 over N runs, one at a time (`bench coldstart`)",
     "p50 42.1-50.0 ms, p99 60.0-73.0 ms (20 runs, all runtimes); earlier 100-run results p50 44-47 ms, p99 59-67 ms",
     "Median meets the 50 ms target; the tail does not on WSL2"],
    ["First response (cold)", "Start until the function's answer arrived (includes upload/import when needed)", "Function snapshot: 73.6-95.7 ms p50",
     "What a caller waits for on a cold call"],
    ["Warm latency", "Round trip of a call served by an idle VM", "0.9-1.9 ms p50 (benchmark); 3.8 ms p50 under load (dashboard)", "~50x faster than a restore"],
    ["Throughput", "Calls per second with 1-32 concurrent k6 virtual users on warm VMs", "600-1,390 calls/s, no failures", "One laptop node serves >1,000 calls/s"],
    ["Density", "Idle VMs started by prewarming until the cap or a 1 GiB memory reserve", "200 (one function), 200 (8 functions), 300 (jailer)", "Limited by the cap, not by memory"],
    ["Memory per VM", "PSS and RSS of each Firecracker process (`/proc/<pid>/smaps_rollup`); host = drop of MemAvailable / VMs", "PSS 2.5-5.2 MiB, host 3.5-3.9 MiB, RSS ~22-23 MiB", "Snapshot pages are shared; RSS overstates"],
    ["Isolation pass rate", "Attacks contained / attacks run (`make test-isolation`)", "6 / 6", "All tested attack classes contained"],
    ["Snapshot build time", "Time to build a function snapshot after deploy", "0.44-0.80 s", "New functions get fast starts within a second"],
    ["Test pass rate", "Passing / total unit tests (`go test ./...`)", "305 / 305", "Regression safety"],
], [2.9, 5.4, 4.3, 3.9], "Evaluation metrics", size=8)

h2("6.3 Results and Discussion")
h3("Cold start")
f_cdf = figure(os.path.join(BENCH, "vm_ready_cdf.png"), 15.5, "Distribution of VM-ready time after a snapshot restore (20 runs per line, demo run of 17 Nov 2026)")
table(["Runtime", "Tier", "VM ready p50", "VM ready p99", "First response p50", "First response p99"], [
    ["Binary", "Boot (no snapshot)", "967.6", "997.6", "986.7", "1014.2"],
    ["Binary", "Restore runtime snapshot", "48.2", "67.9", "102.8", "181.4"],
    ["Binary", "Restore function snapshot", "45.8", "60.0", "77.8", "102.8"],
    ["Binary", "Warm", "-", "-", "1.9", "4.2"],
    ["Python 3.14", "Boot (no snapshot)", "970.6", "985.8", "1037.9", "1051.4"],
    ["Python 3.14", "Restore runtime snapshot", "44.0", "73.0", "101.2", "134.2"],
    ["Python 3.14", "Restore function snapshot", "45.9", "67.0", "73.6", "93.4"],
    ["Python 3.14", "Warm", "-", "-", "0.9", "1.9"],
    ["Node.js 24", "Boot (no snapshot)", "970.5", "1055.4", "1113.4", "1198.7"],
    ["Node.js 24", "Restore runtime snapshot", "50.0", "64.8", "173.8", "208.9"],
    ["Node.js 24", "Restore function snapshot", "42.1", "66.0", "95.7", "118.7"],
    ["Node.js 24", "Warm", "-", "-", "1.1", "2.1"],
], [2.6, 4.2, 2.3, 2.3, 2.6, 2.6], "Cold start results in ms (`make bench`, 17 Nov 2026, 20 runs per mode, one call at a time)", size=8.5)
figure(os.path.join(BENCH, "coldstart.png"), 15.0, "First response by start tier and runtime (bar = p50, tick = p99, log scale)")
figure(os.path.join(BENCH, "restore_breakdown.png"), 15.0, "Where the time of a restore from the runtime snapshot goes (p50 per phase)")
para("**Discussion.** Restoring a snapshot makes a VM ready about **20 times faster** than booting it (≈45 ms instead of "
     "≈970 ms), for every runtime, and the function snapshot removes upload and import from the first call: Python's first "
     "response drops from 101 ms to 74 ms and Node.js's from 174 ms to 96 ms. The **median meets the 50 ms target**; the "
     "99th percentile (60-73 ms) does not. Our measurements in milestone 6 traced the tail to the host: the snapshot "
     "load itself varies between 11 and 40 ms because of KVM scheduling noise under nested virtualization, and the "
     "~520-590 page faults of the first request cost ~37 us each because every fault needs an extra address translation "
     "through Hyper-V. These are properties of running KVM inside WSL2, not of BeeVM's design, which is why the cloud "
     "run without nested virtualization (and REAP-style prefetching [2]) is the next step.")
table(["Milestone (date)", "Runs", "VM ready p50", "VM ready p99", "Note"], [
    ["M6 (27 Oct)", "100", "47.0", "59.1", "First snapshot restore; boot 936.8 ms"],
    ["M6 refine (02 Nov)", "100", "45.2 / 45.8", "65.9 / 67.2", "Agent phase tail fixed; noisy load remains"],
    ["M7 (03 Nov)", "100", "44.2", "60.8", "Warm call p50 1.6 ms, p99 5.0 ms"],
    ["M9 (06 Nov)", "100", "45.2 (none) / 49.0 (jailer)", "65.7 / 77.2", "Isolation costs +3.8 ms at p50"],
    ["M12 (17 Nov)", "20", "42.1-50.0", "60.0-73.0", "All three runtimes, function and runtime snapshots"],
], [3.2, 1.4, 4.0, 3.0, 5.0], "Cold start (VM ready, ms, binary function unless noted) across milestones", size=8.5)
h3("Throughput")
figure(os.path.join(BENCH, "throughput.png"), 15.5, "Throughput and latency on warm VMs with 1-32 concurrent callers (k6, closed loop)")
table(["Virtual users", "Calls/s", "p50 (ms)", "p99 (ms)", "Failed"], [
    ["1", "601", "1.5", "3.5", "0"], ["2", "1,316", "2.0", "5.2", "0"], ["4", "1,026", "3.0", "17.7", "0"],
    ["8", "915", "6.2", "43.6", "0"], ["16", "1,392", "10.9", "33.6", "0"], ["32", "1,351", "18.3", "69.2", "0"],
], [3, 3, 3, 3, 3], "Warm throughput (k6, demo run with 5-second stages)", size=9)
para("One node answered between **600 and 1,390 calls per second** with no failed call. Throughput does not grow "
     "smoothly with concurrency: the k6 load generator, the server and all VMs share the same 16 virtual CPUs of the "
     "laptop, and the demo run uses short 5-second stages, so the numbers are noisy. Latency grows with concurrency "
     "(p50 1.5 → 18 ms) as calls queue for CPUs. In open-loop mixed traffic (50 calls/s over the three runtimes) every "
     "call was warm with p50 1.9 ms and p99 3.8 ms. The first demo run exposed a configuration mistake: with "
     "`--max-vms 32`, 32 callers spread over 3 functions needed more VMs than allowed, and the pool spent its time "
     "evicting and restoring (123 calls/s, p50 243 ms); with a cap of 128 the problem disappeared.")
h3("Density")
figure(os.path.join(BENCH, "density.png"), 16.5, "Density: host memory used, memory per VM (PSS solid, host dashed) and warm latency as idle VMs are added")
table(["Scenario", "Idle VMs reached", "PSS per VM (MiB)", "RSS per VM (MiB)", "Host per VM (MiB)", "Warm p50 / p99 (ms)"], [
    ["One function (all VMs share one snapshot)", "200 (cap)", "2.5", "22.2", "3.5", "3.2 / 31.7"],
    ["Eight different functions", "200 (cap)", "3.6", "23.9", "3.7", "5.3 / 37.8"],
    ["One function, `--isolation jailer` (own jail, cgroup, UID, netns)", "300 (cap)", "5.2", "23.2", "3.9", "2.8 / 34.0"],
], [5.0, 2.4, 2.2, 2.2, 2.2, 2.6], "Density results (no failed calls in any scenario)", size=8.5)
para("Every scenario stopped at the configured cap, **not** at the memory limit: after 300 isolated VMs the node still had "
     "about 5 GiB free. An idle 128 MiB VM costs only **3.5-3.9 MiB of real host memory**, because a restored guest only "
     "touches a few megabytes of its memory and VMs of the same function share the pages of one snapshot file (PSS "
     "falls from 7.0 to 2.5 MiB per VM as VMs are added). RSS (~22 MiB) counts shared pages in every VM and overstates "
     "the cost. At the measured host cost, the remaining memory above a 1 GiB reserve would hold roughly 1,300 idle "
     "VMs; this is an **extrapolation**, not a measurement, and other limits (CPU, file descriptors, network namespaces) "
     "may come first. Warm latency stayed in the low milliseconds as VMs were added.")
h3("Isolation")
table(["Attack", "Stopped by", "Observed (17 Nov 2026, `make test-isolation`)", "Pass"], [
    ["Host file read: search for a host canary, read /etc/shadow, write the disk, replace agent files", "MicroVM boundary; guest user fn; jailer chroot, own UID, seccomp",
     "85 files scanned, canary found 0 times; 4/4 tricks denied; function uid 1000; Firecracker uid 900003, seccomp mode 2, no capabilities", "Yes"],
    ["Cross-tenant network probe", "Network namespace per VM; nftables per-tenant sets; VM->host dropped",
     "Host via gateway and host address blocked; other tenant blocked both ways; same tenant connected (control); scan of 10.200.0.0/24 finds only its own VM", "Yes"],
    ["Fork bomb (escapes its process group with setsid)", "Guest process limit (256), agent kills leftovers; host pids.max 32",
     "10 started, 3,296 refused, at most 55 processes; 1 left afterwards; neighbour median 2.9 ms before, 2.2 ms during", "Yes"],
    ["Memory hog (touch 1 GiB in a 128 MiB VM)", "Guest RAM size and OOM killer (agent protected); host memory.max",
     "Function killed in the guest; same VM served the next call; Firecracker peak 95 of 192 MiB; host OOM kills 0", "Yes"],
    ["CPU hog (busy loop 4 s)", "cgroup cpu.max (`--cpu-limit 50`)", "0.50 CPU over 4.33 s, throttled 43 times; neighbour median 2.6 -> 1.9 ms", "Yes"],
    ["Host memory limit below guest size", "cgroup memory.max on Firecracker", "Host killed that VM; server answered 502; VM removed; neighbour call ok", "Yes"],
], [3.6, 4.0, 7.6, 1.2], "Tenant isolation results (two tenants: attacker and neighbour; jailer mode)", size=8)
para("All six attack classes were contained, each by the layer designed for it, and the neighbour tenant's latency "
     "did not get worse during any attack. Isolation has a cost: in milestone 9 the jailer and per-VM network added "
     "3.8 ms to the median VM-ready time, and per VM it uses about 0.4 MiB more host memory (Table 16).")

h2("6.4 Validation / Demonstration Evidence")
para("The system can be demonstrated from a clean checkout on any Linux machine with KVM (or WSL2 with nested "
     "virtualization) with these commands:")
code(["make setup              # once: Go, Firecracker, kernel and images (asks for sudo)",
      "make demo               # build, doctor, start the server, deploy 3 functions, call each",
      "./bin/beevm invoke hello-python --payload '{\"name\":\"you\"}' -v",
      "make monitoring         # optional: Prometheus :9090 + Grafana :3000",
      "make ui                 # optional: web dashboard at http://127.0.0.1:8080/ui/",
      "BENCH_QUICK=1 make bench   # 5-minute benchmark: charts + report.md in bench/results/m12/<time>/",
      "make test-isolation     # isolation tests (sudo); make bench-jailer for isolated density"])
para("Evidence that the features work: the terminal outputs and screenshots in Section 5.4 (captured from the live "
     "system), the benchmark report and charts in Section 6.3 (generated by `make bench`), the isolation table "
     "(generated by `make test-isolation`), and the test summaries in Appendix A. The source code is kept in a Git "
     "repository (Go module `github.com/om8P/BeeVM`, 31 commits). No passwords, API keys or personal data appear in this "
     "report; keys in the screenshots are masked.")

# ================================================================= 7 SUSTAINABILITY
h1("7. Sustainability and SDG Alignment")
para("BeeVM is infrastructure software, so its sustainability effect comes from how efficiently it uses servers. "
     "We claim only what our measurements support and label the rest as indirect or proposed.")
h2("7.1 Sustainability Assessment")
table(["Dimension", "Questions to Consider", "Project-Specific Assessment / Evidence"], [
    ["Environmental", "Energy and compute use; e-waste; resource efficiency; emissions; lifecycle impacts.",
     "**Positive:** an idle VM costs ~3.5-3.9 MiB of host memory and no CPU (its vCPU is halted), so one node can hold "
     "hundreds of isolated functions; snapshot pages are shared between VMs of a function; idle VMs are recycled after the "
     "keep-alive. Higher density means fewer servers for the same workload. **Negative:** each function snapshot uses "
     "128 MiB of disk; keeping VMs warm trades memory for latency; benchmarks use compute (a demo run takes ~5 minutes). "
     "**Not measured:** energy per call (proposed indicator in 7.4)."],
    ["Social", "Accessibility and inclusion; user safety; privacy; fairness; digital divide; community benefit.",
     "**Safety and privacy:** strong isolation protects each tenant's code and data from other tenants (6/6 attacks "
     "contained); API keys are stored only as hashes; no personal data is collected. **Inclusion:** runs on an ordinary "
     "laptop with free software, so students can study real cloud internals; the dashboard uses a colour-blind-safe "
     "palette with text labels on every colour. **Risk:** a shared platform concentrates many tenants on one machine, "
     "so a missed isolation bug would affect many users (mitigated by defence in depth)."],
    ["Economic", "Development and operating cost; maintenance; affordability; productivity; long-term viability.",
     "**Cost:** ₹0 for software (all open source) and no cloud spending so far; development used an existing laptop. "
     "**Operating cost:** per-call billing becomes cheaper when more functions fit on a server and cold starts are short "
     "(less idle capacity needed). **Maintainability:** small modules, 305 unit tests plus VM-level tests, pinned "
     "versions with checksums, optional modules that can be removed."],
], [2.6, 4.4, 9.5], "Sustainability assessment", size=8.5, first_col_bold=True)

h2("7.2 UN Sustainable Development Goals (SDGs)")
p = d.add_paragraph(style="normal"); p.alignment = WD_ALIGN_PARAGRAPH.CENTER
for n in ("09", "12", "13"):
    p.add_run().add_picture(os.path.join(A, "sdg", "sdg-%s.png" % n), width=Cm(3.0))
    p.add_run("     ")
fmt(p, after=2, align=WD_ALIGN_PARAGRAPH.CENTER, keep_next=True)
caption("Figure", "SDG icons for the goals this project aligns with (source: United Nations, used under the UN SDG icon guidelines)")
table(["SDG No. and Name", "Project Feature / Activity Linked to SDG", "Expected or Observed Contribution", "Evidence / Indicator"], [
    ["**SDG 9: Industry, Innovation and Infrastructure** (targets 9.4 resource-efficient infrastructure, 9.5 technological capability)",
     "Open, reproducible serverless infrastructure combining VM isolation with fast snapshot starts; one-command benchmarks",
     "**Observed:** a working FaaS engine on commodity hardware; cold-start median ≈45 ms; knowledge that others can rebuild and extend",
     "305 unit tests, VM-level tests, `make bench` report; Section 6"],
    ["**SDG 12: Responsible Consumption and Production** (target 12.2 efficient use of resources)",
     "High MicroVM density through shared snapshot pages, small guests and recycling of idle VMs",
     "**Observed:** 200-300 isolated idle VMs on one laptop at 3.5-3.9 MiB host memory each; fewer servers needed for the same functions",
     "Host memory per VM and idle VMs per node (Table 16)"],
    ["**SDG 13: Climate Action** (indirect only)",
     "Fewer servers and less idle capacity can mean less energy and emissions per call",
     "**Expected, not demonstrated:** we did not measure energy",
     "Proposed: energy per 1,000 invocations (Section 7.4)"],
], [3.8, 4.4, 4.6, 3.7], "SDG alignment", size=8.5)

h2("7.3 Sustainability Measures and Trade-offs")
for t in ["**Share instead of copy:** one read-only root filesystem per runtime and one snapshot memory file per function are "
          "shared by all VMs (copy-on-write), which is why PSS per VM falls as VMs are added.",
          "**Recycle idle capacity:** VMs idle longer than the keep-alive are destroyed; the keep-alive length is the trade-off "
          "between memory (and energy) and the chance of a warm call [5].",
          "**Build once, start many times:** function snapshots cost ~0.5 s of work once per deploy and save the import time "
          "on every later cold start.",
          "**Do not over-provision:** `--max-vms` and `--max-vms-per-tenant` cap what one node and one tenant can use.",
          "**Trade-off:** snapshots use disk (128 MiB per function); Firecracker diff snapshots or deleting snapshots of rarely "
          "used functions would reduce this (future work)."]:
    bullet(t)
h2("7.4 Sustainability Indicator / Follow-up")
table(["Indicator", "Status", "How to measure"], [
    ["Host memory per idle VM (MiB)", "Measured: 3.5-3.9 MiB", "`beevmd bench density` (MemAvailable drop / VMs)"],
    ["Idle VMs per node", "Measured: ≥ 300 (cap reached, not memory)", "`make bench`, `make bench-jailer`"],
    ["Energy per 1,000 invocations (J)", "Proposed future measure", "Intel RAPL counters on a bare-metal node during `make bench` (not possible inside WSL2)"],
    ["Disk used by snapshots per function (MiB)", "Measured: 128 MiB (= VM memory size)", "`beevmd snapshot ls`"],
], [5.0, 4.6, 6.9], "Sustainability indicators", size=8.5, first_col_bold=True)

# ================================================================= 8 FEASIBILITY
h1("8. Feasibility, Limitations and Risk Considerations")
h2("8.1 Feasibility")
para("**Technical feasibility** is demonstrated: every component runs, and the hardest parts (snapshot restore, clone "
     "safety, isolation) are covered by tests that boot real VMs. **Time feasibility:** the project was planned as 13 "
     "milestones of about one session each and 12 were finished in 27 days. **Operational feasibility:** `make demo` and "
     "`beevmd doctor` let a new user start the system in minutes; normal operation needs no root. **Cost:** see the "
     "budget below.")
table(["Item / Resource", "Estimated Cost (₹)", "Basis / Notes"], [
    ["Hardware / hosting / cloud", "0", "Existing student laptop; no cloud resources used so far. The planned cloud run (M13) would rent one nested-virtualization VM for a few hours, priced at run time."],
    ["Software / API / dataset", "0", "All open source: Go, Firecracker, Linux, Alpine, Python, Node.js, SQLite, MinIO, Prometheus, Grafana, React, k6, matplotlib"],
    ["Other resources", "0", "Electricity of the laptop during development (not metered)"],
    ["Total estimated cost", "0", "One-time and recurring costs so far are zero; a production node would add server and storage costs"],
], [4.6, 3.0, 8.9], "Budget estimate", size=8.5, first_col_bold=True)

h2("8.2 Limitations")
for t in ["**Nested virtualization:** all numbers come from KVM inside WSL2. Page faults and snapshot loads are slower and "
          "noisier than on bare metal; the p99 cold start (60-73 ms) misses the 50 ms target there.",
          "**Sample size:** the latest benchmark is a 5-minute demo run with 20 runs per mode; 100-run results exist from earlier "
          "milestones (Table 14) but not for every runtime in the final version.",
          "**Density** was measured with idle VMs of small \"hello\" functions and stopped at the configured caps (200/300); "
          "busy VMs and larger functions use more memory. The ~1,300-VM figure is an extrapolation.",
          "**Throughput** shares 16 virtual CPUs between the load generator and the server, so it understates a real node.",
          "**Single node only:** no multi-node scheduling, autoscaling or failover.",
          "**Missing features:** per-function environment variables and secrets, stored logs (only the last 4 KB per call), "
          "event triggers other than HTTP.",
          "**Not yet verified:** the Docker Compose profile for monitoring, the dashboard in browsers other than Chromium, the "
          "cloud run (milestone 13)."]:
    bullet(t)

h2("8.3 Risks, Ethics, Privacy and Security")
table(["Risk", "Safeguard in BeeVM", "Residual risk"], [
    ["Tenant escape or cross-tenant access", "Hardware VM boundary; jailer chroot + unique UID + seccomp; cgroups; netns + nftables; guest user `fn`", "Bugs in KVM/Firecracker; mitigated by updates"],
    ["Resource exhaustion by one tenant", "cpu.max, memory.max, pids.max, guest process limit, `--max-vms-per-tenant`, function timeouts", "Disk use by snapshots is not limited per tenant"],
    ["Stolen API keys", "Random keys shown once, stored as SHA-256 hashes; CLI config file mode 0600; dashboard keeps the key in session storage", "No key expiry or scopes yet"],
    ["Cloned VM state after restore (same clock, same random numbers)", "Agent `/resume` sets the clock and reseeds entropy; Firecracker VMGenID also reseeds", "Applications that cache random values before the snapshot"],
    ["Data left in a warm VM", "`/tmp` persists only between calls of the same function (as in AWS Lambda), never across functions or tenants", "Developers must not assume a clean `/tmp`"],
    ["Information leak through metrics", "`/metrics` and `--addr` default to localhost", "Exposed if an operator binds to a public address"],
    ["Weakening the host for performance", "We refused to enable unprivileged userfaultfd on the laptop", "Prefetch work must run on a dedicated node"],
    ["Supply chain", "Pinned versions and SHA-256 checksums for downloaded tools; lockfile for the dashboard", "Trust in upstream projects"],
], [4.0, 7.2, 5.3], "Risks and safeguards", size=8.5, first_col_bold=True)

# ================================================================= 9 CONCLUSION
h1("9. Conclusion and Future Scope")
h2("9.1 Summary")
para("We built **BeeVM**, a working multi-tenant Function-as-a-Service engine that runs every function in its own "
     "Firecracker MicroVM. Against the case-study objectives:")
table(["Objective", "Result", "Met?"], [
    ["Cold start < 50 ms via snapshot restore", "VM ready p50 42-50 ms (≈20x faster than boot); p99 60-73 ms on WSL2", "Median: yes. Tail: not on WSL2"],
    ["Tenant isolation verified by tests", "6 of 6 attack classes contained; neighbours unaffected", "Yes"],
    ["MicroVM density per node", "200 idle VMs (300 with full isolation) at 3.5-3.9 MiB host memory each; cap reached, not memory", "Yes (measured and reported)"],
    ["Warm pool for fast repeat calls", "Warm calls 0.9-1.9 ms p50", "Yes"],
    ["Three runtimes with function snapshots", "Binary, Python 3.14, Node.js 24", "Yes"],
    ["Observability and dashboards", "Prometheus, Grafana, live web dashboard", "Yes"],
    ["Reproducible evaluation", "`make bench` in ~5 minutes, isolation tests, 305 unit tests", "Yes"],
], [5.0, 8.0, 3.5], "Objectives and outcomes", size=8.5, first_col_bold=True)
para("The project showed in practice why snapshots are the key to fast VM-based serverless computing, where the "
     "remaining milliseconds go (page faults and host noise), and that strong isolation does not have to cost much "
     "density. Its sustainability contribution is efficiency: many isolated functions on one machine (SDG 9, SDG 12), "
     "with energy savings (SDG 13) expected but not yet measured.")
h2("9.2 Future Scope")
for t in ["**Cloud run (milestone 13):** repeat `make bench` on a node without nested virtualization (Terraform-created) and "
          "compare with WSL2.",
          "**Page prefetch (REAP [2]):** record each function's working set and load it in one step at restore through "
          "userfaultfd, targeting the remaining page-fault cost and the p99.",
          "**Adaptive keep-alive and prewarming** from per-function call history [5]; a warm-up call before freezing a function snapshot.",
          "**Diff snapshots and snapshot eviction** to cut the 128 MiB of disk per function.",
          "**Multi-node scheduling** with snapshot distribution through S3, and autoscaling.",
          "**Developer features:** environment variables and secrets per function, stored logs (`beevm logs`), event triggers.",
          "**Energy measurement** (RAPL) to turn the SDG 13 link into a measured indicator."]:
    bullet(t)

# ================================================================= REFERENCES
h1("References")
refs = [
    "A. Agache, M. Brooker, A. Florescu, A. Iordache, A. Liguori, R. Neugebauer, P. Piwonka and D.-M. Popa, \"Firecracker: Lightweight Virtualization for Serverless Applications,\" in Proc. 17th USENIX Symposium on Networked Systems Design and Implementation (NSDI '20), Santa Clara, CA, USA, 2020, pp. 419-434. https://www.usenix.org/conference/nsdi20/presentation/agache",
    "D. Ustiugov, P. Petrov, M. Kogias, E. Bugnion and B. Grot, \"Benchmarking, Analysis, and Optimization of Serverless Function Snapshots,\" in Proc. 26th ACM Int. Conf. on Architectural Support for Programming Languages and Operating Systems (ASPLOS '21), 2021, pp. 559-572, doi: 10.1145/3445814.3446714.",
    "D. Du, T. Yu, Y. Xia, B. Zang, G. Yan, C. Qin, Q. Wu and H. Chen, \"Catalyzer: Sub-millisecond Startup for Serverless Computing with Initialization-less Booting,\" in Proc. ASPLOS '20, 2020, pp. 467-481, doi: 10.1145/3373376.3378512.",
    "E. Oakes, L. Yang, D. Zhou, K. Houck, T. Harter, A. C. Arpaci-Dusseau and R. H. Arpaci-Dusseau, \"SOCK: Rapid Task Provisioning with Serverless-Optimized Containers,\" in Proc. USENIX Annual Technical Conference (USENIX ATC '18), Boston, MA, USA, 2018, pp. 57-70.",
    "M. Shahrad, R. Fonseca, I. Goiri, G. Chaudhry, P. Batum, J. Cooke, E. Laureano, C. Tresness, M. Russinovich and R. Bianchini, \"Serverless in the Wild: Characterizing and Optimizing the Serverless Workload at a Large Cloud Provider,\" in Proc. USENIX ATC '20, 2020. arXiv:2003.03423.",
    "L. Wang, M. Li, Y. Zhang, T. Ristenpart and M. Swift, \"Peeking Behind the Curtains of Serverless Platforms,\" in Proc. USENIX ATC '18, Boston, MA, USA, 2018, pp. 133-146.",
    "F. Manco, C. Lupu, F. Schmidt, J. Mendes, S. Kuenzer, S. Sati, K. Yasukata, C. Raiciu and F. Huici, \"My VM is Lighter (and Safer) than your Container,\" in Proc. 26th Symposium on Operating Systems Principles (SOSP '17), Shanghai, China, 2017, pp. 218-233, doi: 10.1145/3132747.3132763.",
    "Firecracker project, \"Firecracker documentation: snapshot support, jailer, vsock,\" GitHub. https://github.com/firecracker-microvm/firecracker/tree/main/docs (accessed 17 Nov 2026).",
    "The Linux kernel documentation, \"Control Group v2.\" https://docs.kernel.org/admin-guide/cgroup-v2.html (accessed 17 Nov 2026).",
    "Prometheus Authors, \"Prometheus documentation.\" https://prometheus.io/docs/ (accessed 17 Nov 2026); Grafana Labs, \"Grafana documentation.\" https://grafana.com/docs/grafana/latest/ (accessed 17 Nov 2026).",
    "Grafana Labs, \"Grafana k6 documentation.\" https://grafana.com/docs/k6/latest/ (accessed 17 Nov 2026).",
    "MinIO, \"MinIO object storage for Linux.\" https://min.io/docs/minio/linux/index.html (accessed 17 Nov 2026).",
    "United Nations, \"Sustainable Development Goals\" and \"Guidelines for the use of the SDG logo including the colour wheel, and 17 icons.\" https://www.un.org/sustainabledevelopment/ (accessed 17 Nov 2026).",
    "Advanced Cloud System Software Design: 30 Case Studies, Case Study 06: Lightweight MicroVM FaaS Runtime Engine, course document, Department of Computer Engineering, VESIT, 2026.",
]
for i, r_ in enumerate(refs, 1):
    p = d.add_paragraph(style="normal")
    p.paragraph_format.left_indent = Cm(0.9)
    p.paragraph_format.first_line_indent = Cm(-0.9)
    r = p.add_run("[%d]  " % i); set_run_font(r, 10)
    rich(p, r_, 10)
    fmt(p, after=4, align=WD_ALIGN_PARAGRAPH.LEFT)

# ================================================================= APPENDICES
h1("Appendices")
h2("Appendix A: Additional screenshots / outputs")
figure(os.path.join(A, "term", "t09-tests.png"), 16.5, "Test summary: 305 unit tests passed, 0 failed; benchmark tool tests; number of KVM integration tests")
figure(os.path.join(A, "term", "t10-gotest.png"), 12.5, "`go test ./...` per package (no KVM needed)")
figure(os.path.join(A, "term", "t11-bench-coldstart.png"), 15.0, "Raw output of the cold-start benchmark for the binary runtime inside `make bench`")
figure(os.path.join(A, "term", "t12-bench-density.png"), 16.5, "Raw output of the one-function density run inside `make bench`")
figure(os.path.join(A, "term", "t05-snapshots.png"), 16.5, "`beevmd snapshot ls`: runtime and function snapshots with build times")
figure(os.path.join(A, "term", "t08-serve-log.png"), 16.5, "Server request log (text format) during the load run")
figure(os.path.join(A, "term", "t13-git-log.png"), 15.0, "Git history of the project (commit times in IST): evidence for the work log in Table 7")
h2("Appendix B: Selected pseudocode")
para("The pseudocode for invocation handling and function snapshot building is in Section 4.5. The warm pool's "
     "acquire step is:")
code(["function Acquire(key):",
      "    lock",
      "    loop:",
      "        if idle VM with this key exists: mark Busy, unlock, return it          # warm",
      "        if used VMs < max-vms and tenant below its cap: reserve slot, unlock, create VM, return it",
      "        if an idle VM of any key exists (of this tenant if it is at its cap): evict least recently used, continue",
      "        wait on the broadcast channel (up to QueueWait), else return ErrBusy (HTTP 503)"])
h2("Appendix C: Additional test information")
para("Unit tests: `make test` (all packages; `-race` for pool, runner, snapshot, api, events, metrics and the commands). "
     "VM-level tests: `make test-kvm` (build tag `kvm`; boots, snapshots and restores real MicroVMs, end-to-end API tests "
     "for every runtime, warm pool, metrics, events). Isolation tests: `make test-isolation` (root; writes the isolation "
     "table). Storage: `make test-s3` against MinIO. Dashboard: `make test-ui` (Vitest). Benchmark tools: `make test-bench`. "
     "During the preparation of this report one unit test (`TestMissingKeyIsExitCode2`) failed because it read the "
     "developer's saved CLI login; the test was fixed to use an empty configuration file and all 305 tests pass.")
h2("Appendix D: Repository / demo / deployment information")
table(["Item", "Value"], [
    ["Repository", "Git, 31 commits from 22 Oct 2026 to 17 Nov 2026; Go module `github.com/om8P/BeeVM`"],
    ["Main documents", "`README.md`, `docs/architecture.md`, `docs/milestones.md`, `docs/progress.md`, `developer-guide/guide.md`"],
    ["Build and run", "`make setup`, `make build`, `make demo`, `make monitoring`, `make ui`"],
    ["Benchmarks and tests", "`make bench` (`BENCH_QUICK=1`), `make bench-jailer`, `make test`, `make test-kvm`, `make test-isolation`"],
    ["Results used in this report", "`bench/results/m12/20261117-090325/` (cold start, k6, density, jailer density), `bench/results/isolation-20261117-085503.md`"],
    ["Credentials", "None included. API keys are created with `beevmd tenant add` and shown only once."],
], [4.2, 12.3], "Repository and reproduction information", size=8.5, first_col_bold=True)

d.save(OUT)
print("saved", OUT, "figures:", COUNT["Figure"], "tables:", COUNT["Table"])
