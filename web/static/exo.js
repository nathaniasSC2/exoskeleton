// Live table: poll for rows newer than the newest one shown. The DOM is capped
// at 200 rows and polling pauses while the tab is hidden, so an open tab left
// for days costs nothing.
(function () {
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-confirm]");
    if (b && !confirm(b.dataset.confirm)) { e.preventDefault(); return; }
    // Power switches: one line on why, for the switch ledger (Esc cancels).
    var p = e.target.closest("[data-why]");
    if (p && p.form) {
      var why = prompt(p.dataset.why + "\nWhy? One line for the switch ledger (optional):", "");
      if (why === null) { e.preventDefault(); return; }
      var f = p.form.querySelector("input[name=why]");
      if (f) f.value = why;
    }
  });
  var table = document.getElementById("turns");
  if (!table || !table.dataset.live) return;
  var body = table.tBodies[0];
  function newest() {
    var r = body.querySelector("tr[data-id]");
    return r ? r.dataset.id : "0";
  }
  function tick() {
    if (document.hidden) return;
    fetch("/api/turns?after=" + newest() + "&" + (table.dataset.q || ""))
      .then(function (r) { return r.json(); })
      .then(function (rows) {
        if (!rows || !rows.length) return;
        var empty = body.querySelector(".empty");
        if (empty) empty.parentNode.remove();
        for (var i = rows.length - 1; i >= 0; i--) {
          var old = body.querySelector('tr[data-id="' + rows[i].id + '"]');
          if (old) old.remove();
          body.insertAdjacentHTML("afterbegin", rows[i].html);
          body.firstElementChild.classList.add("new");
        }
        while (body.rows.length > 200) body.deleteRow(-1);
      })
      .catch(function () {});
  }
  setInterval(tick, 4000);
  // Rows still running get re-fetched when they finish: refresh them by id.
  setInterval(function () {
    if (document.hidden) return;
    var running = body.querySelector("tr.st-running");
    if (!running) return;
    var id = running.dataset.id;
    fetch("/api/turns?after=" + (id - 1) + "&" + (table.dataset.q || ""))
      .then(function (r) { return r.json(); })
      .then(function (rows) {
        (rows || []).forEach(function (row) {
          var old = body.querySelector('tr[data-id="' + row.id + '"]');
          if (old) old.outerHTML = row.html;
        });
      })
      .catch(function () {});
  }, 3000);
})();
