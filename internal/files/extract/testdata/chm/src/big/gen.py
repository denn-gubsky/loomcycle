# Writes the pages of big.chm: twelve chapters of generated words, enough
# text to fill several LZX frames and reset intervals. Deterministic, so a
# rebuild makes the same pages.
import sys

words = ("lamp wick glass oil ledger keeper column total entry week page line "
         "brass hook shelf candle smoke flame light dark night morning road "
         "bridge river stone wall gate yard field barn wheel cart horse rope "
         "nail board door window roof chimney stair floor table chair bench "
         "cup plate bowl spoon knife bread salt water tea letter stamp seal "
         "clock bell").split()
state = 12345
def rnd(n):
    global state
    state = (state * 1103515245 + 12345) % 2**31
    return state % n

out = sys.argv[1]
toc = ['<html><body><ul>']
files = []
for ch in range(1, 13):
    name = "ch%02d.htm" % ch
    files.append(name)
    paras = []
    for p in range(40):
        sent = []
        for s in range(6):
            n = 6 + rnd(8)
            ws = [words[rnd(len(words))] for _ in range(n)]
            sent.append(" ".join(ws).capitalize() + ".")
        paras.append("<p>" + " ".join(sent) + "</p>")
    with open("%s/%s" % (out, name), "w") as f:
        f.write("<html><head><title>Chapter %d</title></head><body>\n<h1>Chapter %d</h1>\n%s\n</body></html>\n"
                % (ch, ch, "\n".join(paras)))
    toc.append('<li><object type="text/sitemap"><param name="Name" value="Chapter %d">'
               '<param name="Local" value="%s"></object>' % (ch, name))
toc.append('</ul></body></html>')
with open(out + "/toc.hhc", "w") as f:
    f.write("\n".join(toc) + "\n")
with open(out + "/big.hhp", "w") as f:
    f.write("[OPTIONS]\nCompatibility=1.1 or later\nCompiled file=big.chm\nContents file=toc.hhc\n"
            "Default topic=ch01.htm\nDisplay compile progress=No\nLanguage=0x409 English (United States)\n"
            "Title=The Long Ledger\n\n[FILES]\n" + "\n".join(files) + "\n")
