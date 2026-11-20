// Applies the saved appearance before the first paint, so a dark page never
// flashes light. app.js owns the control; this only reads the stored choice.
(function () {
  var choice = "system";
  try {
    choice = localStorage.getItem("runquill-theme") || localStorage.getItem("runquill-login-theme") || "system";
  } catch (error) {}
  document.documentElement.dataset.theme = choice === "light" || choice === "dark" ? choice : "system";
})();
