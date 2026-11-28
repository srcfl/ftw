// FTW Cloud's panel in Settings. Project statistics are part of Cloud's
// terms and nothing else: with Cloud on they are shared, without it nothing
// leaves the box (Fredrik, 28 November 2026). The owner accepts the terms
// once, when pairing here, in the app or in the web app, and the box does
// not ask again. Only Cloud under earlier terms offers the current ones, and
// only here, never as a prompt on the Overview.
(function (window, document) {
  "use strict";
  var read = window.fetch.bind(window);
  var details = "Recorded hourly energy, configured battery capacity, battery and charger power limits, and solar capacity help improve FTW and publish group totals. Values are rounded to 10 Wh or W. No names, addresses or device ids.";
  var privacy = "Sourceful links reports to this box for deletion. They are not anonymous and stay for 48 hours. Public values need at least 20 homes, with no home above 30% of a sum. Turning FTW Cloud off deletes unpublished reports when the box reaches Cloud. Published totals remain.";
  var status, policy, authStatus, saving=false, refreshNumber=0;
  function node(tag, text, parent) {
    var el = document.createElement(tag);
    if (text) el.textContent = text;
    if (parent) parent.appendChild(el);
    return el;
  }
  function say(parent, text) { var p=node("p",text,parent);p.className="hint";return p; }
  function button(parent,text,run,primary,feedback) {
    var b=node("button",text,parent);b.type="button";b.className=primary?"btn-send":"btn-ghost";
    b.addEventListener("click",function(){
      if(saving)return;
      saving=true;
      document.querySelectorAll(".project-statistics-panel button").forEach(function(button){button.disabled=true;});
      b.disabled=true;
      b.textContent="Saving…";
      feedback.textContent="";
      feedback.hidden=true;
      Promise.resolve().then(run).then(function(){saving=false;return refresh();}).catch(function(e){
        feedback.textContent=e.message||"The choice could not be saved. Try again.";
        feedback.hidden=false;
      }).finally(function(){
        saving=false;
        document.querySelectorAll(".project-statistics-panel button").forEach(function(button){button.disabled=false;});
        b.disabled=false;b.textContent=text;
      });
    });
  }
  function write(method,path,body) {
    return window.fetch(path,{method:method,headers:{"Content-Type":"application/json"},body:body?JSON.stringify(body):undefined}).then(function(r){return r.json().then(function(v){if(!r.ok)throw Error(v.error||"The box refused the choice.");return v;});});
  }
  function accept() {
    return write("POST","/api/cloud/grant",{scopes:policy.cloud_scopes,policy:policy.cloud_policy});
  }
  // What the statistics in Cloud's terms are, one tap away.
  function cloudScope(parent) {
    var disclosure=node("details",null,parent);
    node("summary","What the statistics share",disclosure);
    say(disclosure,details);
    say(disclosure,privacy);
    var link=node("a","Privacy and Cloud terms",parent);link.href="https://ftw.energy/privacy#statistics";link.target="_blank";link.rel="noopener";
  }
  function signIn(parent) {
    say(parent,authStatus && authStatus.home_assistant
      ?"Home Assistant manages sign-in for this choice."
      :authStatus && authStatus.lan_auth
      ?"Your house password protects this choice. The box will ask you to log in if needed."
      :authStatus && authStatus.lan_auth===false
        ?"No house password is required. With password protection off, anyone who can reach this box on your private network can save this choice."
        :"This choice uses the same sign-in rules as Settings. A house password is optional.");
  }
  function paint(host) {
    host.textContent="";
    if (!status || !policy) {host.hidden=true;return;}
    host.hidden=false;
    var assistant=host.id === "access-cloud-settings";
    var cloud=status.link==="on" && !status.statisticsOnly;
    var panel=node("fieldset",null,host);
    panel.className="project-statistics-panel";
    node("legend",assistant?"FTW Cloud for assistants":"FTW Cloud",panel);
    var lead=node("div",null,panel);
    var actions=node("div",null,panel);actions.className="app-link-actions";
    var feedback=node("p",null,panel);feedback.className="project-statistics-error";feedback.setAttribute("role","alert");feedback.hidden=true;
    if(status.link==="ending") {
      say(lead,"FTW Cloud is stopping and deleting its stored copy. The box keeps working locally.");
      return;
    }
    var off=function(){
      button(actions,"Turn FTW Cloud off",function(){
        if(!window.confirm("Turn FTW Cloud off? The app and assistants lose remote access, and the box stops sending statistics. The box keeps working locally. You may need to pair phones again if you turn Cloud back on."))return;
        return write("DELETE","/api/cloud");
      },false,feedback);
    };
    if(cloud && status.cloudTermsAccepted) {
      say(lead,"FTW Cloud is on. Its terms include project statistics.");
      cloudScope(lead);
      off();
    } else if(cloud) {
      // Cloud under earlier terms: the one place the box offers the current ones.
      say(lead,"FTW Cloud runs under earlier terms. The current terms add project statistics.");
      cloudScope(lead);
      signIn(lead);
      button(actions,"Accept the current terms",accept,true,feedback);
      off();
    } else if(assistant) {
      say(lead,"Your assistant reaches this box through FTW Cloud. Sourceful keeps a readable copy of your energy data, and Cloud's terms include project statistics.");
      cloudScope(lead);
      signIn(lead);
      button(actions,"Accept Cloud terms and turn it on",accept,true,feedback);
    } else {
      say(lead,"FTW Cloud is off. The box works locally and sends no statistics.");
    }
  }
  function refresh() {
    var current=++refreshNumber;
    return Promise.all([read("/api/cloud"),read("/api/cloud/stats-policy"),read("/api/auth/status").catch(function(){return null;})]).then(function(rs){
      return Promise.all(rs.map(function(r){return r && r.ok?r.json().catch(function(){return null;}):null;}));
    }).then(function(v){
      if(current!==refreshNumber || saving || !v[0] || !v[1])return;
      status=v[0];policy=v[1];authStatus=v[2];
      if(window.dispatchEvent && typeof CustomEvent === "function") window.dispatchEvent(new CustomEvent("ftw-cloud-choice-changed", {detail:status}));
      document.querySelectorAll("[data-project-statistics-settings]").forEach(paint);
    }).catch(function(){});
  }
  if(!document.getElementById("project-statistics-styles")) {
    var styles=node("link");styles.id="project-statistics-styles";styles.rel="stylesheet";styles.href="/project-statistics.css";document.head.appendChild(styles);
  }
  window.FTWStatistics={refresh:refresh,cloudAccepted:function(){return !!(status && status.cloudTermsAccepted);},mount:function(host){if(status&&policy)paint(host);refresh();}};
  window.addEventListener("ftw-owner-ready",refresh);
  if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",refresh);else refresh();
})(window,document);
