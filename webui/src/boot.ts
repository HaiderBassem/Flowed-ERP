/**
 * Applied before first paint.
 *
 * Density and theme are read from storage and written onto <html> here rather
 * than in React, so the desk does not render at the default row height and
 * then reflow to compact with a queue waiting. §09's density modes are
 * remembered per operator; the desk defaults to compact and decision screens
 * to comfortable.
 *
 * Dark is never the default (§15): windows are worked in bright daylight, so
 * the absence of a stored choice means "follow the system", not "dark".
 */

const density = localStorage.getItem("flowed.density");
if (density === "compact" || density === "default" || density === "comfortable") {
  document.documentElement.dataset["density"] = density;
}

const nav = localStorage.getItem("flowed.nav");
if (nav === "rail") {
  document.documentElement.dataset["nav"] = "rail";
}

const theme = localStorage.getItem("flowed.theme");
if (theme === "light" || theme === "dark") {
  document.documentElement.dataset["theme"] = theme;
}

export {};
