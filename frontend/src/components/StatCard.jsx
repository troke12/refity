import './StatCard.css';

function StatCard({ icon, label, value, gradient }) {
  return (
    <div className={`stat-card ${gradient} fade-in`}>
      <div className="card-body p-4">
        <div className="d-flex align-items-center">
          <div className={`stat-icon me-3 ${gradient}`}>
            <i className={`bi ${icon}`}></i>
          </div>
          <div className="flex-grow-1">
            <p className="stat-label mb-1">{label}</p>
            <h2 className="stat-value mb-0">{value}</h2>
          </div>
        </div>
      </div>
    </div>
  );
}

export default StatCard;

