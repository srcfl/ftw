(function () {
  'use strict';

  // The refund runs on Swedish calendar years. An owner can apply for the
  // three years before this one, and the running year shows progress.
  var firstYear = 2018;
  var details = document.getElementById('battery-return-details');
  var select = document.getElementById('battery-return-year');
  var csv = document.getElementById('battery-return-csv');
  var result = document.getElementById('battery-return-result');
  var rows = document.getElementById('battery-return-rows');
  var meta = document.getElementById('battery-return-meta');
  if (!details || !select || !result || !rows) return;
  var request = 0;

  var thisYear = Number(new Intl.DateTimeFormat('en', { year: 'numeric', timeZone: 'Europe/Stockholm' }).format(new Date()));
  for (var year = thisYear; year >= Math.max(firstYear, thisYear - 3); year--) {
    var option = document.createElement('option');
    option.value = String(year);
    option.textContent = year === thisYear ? year + ' (so far)' : String(year);
    select.appendChild(option);
  }
  select.value = String(Math.max(firstYear, thisYear - 1));

  // Whole kWh, rounded down: the figure an owner enters never exceeds what
  // the readings show.
  function kWh(wh) {
    return Math.floor(Math.max(0, Number(wh) || 0) / 1000).toLocaleString() + ' kWh';
  }

  function kWhExact(wh) {
    return (Math.max(0, Number(wh) || 0) / 1000).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' kWh';
  }

  // Why a year has no figure, from the battery's configuration.
  var problems = {
    battery_size: function () {
      return 'Set the battery\'s size in Settings to get a figure. FTW needs to know how much it can hold.';
    },
    batteries: function (data) {
      return 'FTW works this figure out for one battery with its size set. In ' + data.year + ' it read ' +
        data.batteries + ' batteries. A replaced battery counts as a second one.';
    }
  };

  function monthName(month) {
    return new Date(Date.UTC(2000, month - 1, 15)).toLocaleDateString(undefined, { month: 'long', timeZone: 'UTC' });
  }

  function cell(row, text) {
    var td = document.createElement('td');
    td.textContent = text;
    row.appendChild(td);
  }

  function render(data) {
    var complete = Number(data.complete_hours) || 0;
    var hours = Number(data.hours) || 0;
    var running = Number(data.year) === thisYear;
    result.textContent = '';
    if (problems[data.problem]) {
      result.className = 'energy-history-quality-note';
      result.textContent = problems[data.problem](data);
    } else if (!complete) {
      result.className = 'energy-history-quality-note';
      result.textContent = 'FTW has no complete readings for ' + data.year + '.';
    } else {
      result.className = 'energy-history-quality-note' + (complete === hours && !running ? ' good' : '');
      var figure = document.createElement('strong');
      figure.textContent = kWh(data.returned_wh);
      result.appendChild(figure);
      result.appendChild(document.createTextNode(
        ' of grid electricity went back to the grid after battery storage in ' + data.year + '. ' +
        'Readings are complete for ' + complete.toLocaleString() + ' of ' + hours.toLocaleString() + ' hours' +
        (running ? ' so far; the year is still running.' : '.') +
        ' Counted with a ' + kWhExact(data.capacity_wh) + ' battery and at least ' +
        Math.floor((Number(data.efficiency) || 0) * 100) + ' % of its charge coming back out.'
      ));
    }
    rows.textContent = '';
    (data.months || []).forEach(function (month) {
      if (!month.hours) return;
      var row = document.createElement('tr');
      cell(row, monthName(month.month));
      cell(row, month.complete_hours ? kWh(month.returned_wh) : 'No readings');
      cell(row, Number(month.complete_hours).toLocaleString() + ' of ' + Number(month.hours).toLocaleString());
      rows.appendChild(row);
    });
    if (meta) meta.textContent = problems[data.problem] ? 'No figure' : complete ? kWh(data.returned_wh) + ' in ' + data.year : 'No readings';
  }

  function load() {
    var current = ++request;
    var year = select.value;
    if (csv) csv.href = '/api/energy/battery-return.csv?year=' + encodeURIComponent(year);
    result.className = 'energy-history-quality-note';
    result.textContent = 'Working out ' + year + '…';
    rows.textContent = '';
    if (meta) meta.textContent = '';
    fetch('/api/energy/battery-return?year=' + encodeURIComponent(year)).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (data) {
        if (!response.ok) throw new Error(data.error || 'The figure for ' + year + ' is unavailable.');
        return data;
      });
    }).then(function (data) {
      if (current === request) render(data);
    }).catch(function (error) {
      if (current !== request) return;
      result.className = 'energy-history-quality-note';
      result.textContent = error.message;
    });
  }

  details.addEventListener('toggle', function () {
    if (details.open) load();
  });
  select.addEventListener('change', load);
})();
