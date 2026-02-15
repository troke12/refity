(function () {
  var toggle = document.getElementById('navToggle');
  var mobile = document.getElementById('navMobile');
  var icon = toggle ? toggle.querySelector('i') : null;

  function closeMenu() {
    if (mobile) mobile.classList.remove('open');
    if (icon) {
      icon.classList.remove('bi-x-lg');
      icon.classList.add('bi-list');
    }
  }

  function openMenu() {
    if (mobile) mobile.classList.add('open');
    if (icon) {
      icon.classList.remove('bi-list');
      icon.classList.add('bi-x-lg');
    }
  }

  function isOpen() {
    return mobile && mobile.classList.contains('open');
  }

  if (toggle && mobile) {
    toggle.addEventListener('click', function () {
      if (isOpen()) closeMenu();
      else openMenu();
    });

    mobile.querySelectorAll('a').forEach(function (a) {
      a.addEventListener('click', closeMenu);
    });
  }
})();
