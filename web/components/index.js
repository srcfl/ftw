// Single entry point that registers every ftw-* custom element. Load
// this once from index.html with <script type="module"> — individual
// component files are imported here, not by the page directly, so new
// components get picked up by adding one line below.

import "./ftw-element.js";
// Price units land on window.FTWUnits for the classic scripts that can't
// import them; the import has to happen whether or not a component uses it.
import { bootstrapCurrency } from "./price-units.js";
// One request, so a view that shows prices without fetching them still
// labels them in the household's own currency.
bootstrapCurrency();
// Foundations — registered as each lands.
import "./ftw-modal.js";
import "./ftw-progress-bar.js";
import "./ftw-badge.js";
import "./ftw-card.js";
import "./ftw-tabs.js";
import "./ftw-legend.js";
import "./ftw-energy-flow.js";
import "./energy-flow-readings.js";
import "./ftw-battery-control.js?v=apifetch1";
import "./ftw-pv-control.js?v=apifetch1";
import "./ftw-price-chart.js?v=zones1";
import "./ftw-energy-cake.js";
import "./ftw-bar-chart.js";
import "./ftw-history-card.js?v=apiread2";
import "./ftw-savings-card.js?v=zones1";
import "./ftw-update-check.js?v=apifetch1";
import "./ftw-notif-status.js?v=apifetch1";
import "./ftw-notif-test-button.js";
import "./ftw-notif-history.js?v=apifetch1";
